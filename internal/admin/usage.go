package admin

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/dio/rute/internal/callstore"
	"github.com/dio/rute/internal/pricing"
)

// usageWindows are the ranges the usage report accepts, with the grouping
// used when none is given.
type usageWindow struct {
	name     string
	duration time.Duration
	by       string
}

var usageWindows = []usageWindow{
	{"24h", 24 * time.Hour, "hour"},
	{"7d", 7 * 24 * time.Hour, "day"},
	{"30d", 30 * 24 * time.Hour, "day"},
}

type usageGrouping struct {
	name string
	step time.Duration
}

var usageGroupings = []usageGrouping{
	{"hour", time.Hour},
	{"day", 24 * time.Hour},
	{"total", 0},
}

type usageQuery struct {
	window   string
	since    time.Time
	by       string
	step     time.Duration
	location *time.Location
}

func parseUsageQuery(request *http.Request) (usageQuery, error) {
	query := usageQuery{window: request.URL.Query().Get("window"), by: request.URL.Query().Get("by")}
	if query.window == "" {
		query.window = "7d"
	}
	index := slices.IndexFunc(usageWindows, func(window usageWindow) bool { return window.name == query.window })
	if index < 0 {
		return usageQuery{}, errors.New("window must be 24h, 7d, or 30d")
	}
	window := usageWindows[index]
	if query.by == "" {
		query.by = window.by
	}
	grouping := slices.IndexFunc(usageGroupings, func(grouping usageGrouping) bool { return grouping.name == query.by })
	if grouping < 0 {
		return usageQuery{}, errors.New("by must be hour, day, or total")
	}
	query.step = usageGroupings[grouping].step
	query.location = time.Local
	now := time.Now().In(query.location)
	// Start at the beginning of a period so the first row is complete.
	switch query.step {
	case time.Hour:
		query.since = now.Add(-window.duration).Truncate(time.Hour)
	default:
		start := now.Add(-window.duration)
		query.since = time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, query.location)
	}
	return query, nil
}

// UsageRow is one row of GET /v1/usage. CostUSD is null when the model has no
// price.
type UsageRow struct {
	Start            *time.Time `json:"start,omitempty"`
	Provider         string     `json:"provider"`
	Account          string     `json:"account"`
	Model            string     `json:"model"`
	Calls            int64      `json:"calls"`
	OK               int64      `json:"ok"`
	RateLimited      int64      `json:"rate_limited"`
	InputTokens      int64      `json:"input_tokens"`
	OutputTokens     int64      `json:"output_tokens"`
	CacheReadTokens  int64      `json:"cache_read_tokens"`
	CacheWriteTokens int64      `json:"cache_write_tokens"`
	CostUSD          *float64   `json:"cost_usd"`
}

// usageReport is the usage for one query with estimated costs.
type usageReport struct {
	rows       []UsageRow
	unpriced   []string
	pricesPath string
	pricesErr  error
}

func (server *Server) usage(ctx context.Context, query usageQuery) (usageReport, error) {
	if server.config.Calls == nil {
		return usageReport{}, errHistoryDisabled
	}
	counters, err := server.config.Calls.Usage(ctx, query.since, query.step)
	if err != nil {
		return usageReport{}, err
	}
	var report usageReport
	var table pricing.Table
	if server.config.Prices != nil {
		report.pricesPath = server.config.Prices.Path()
		table, report.pricesErr = server.config.Prices.Table()
	}
	for _, item := range counters {
		row := usageRow(item)
		if rate, ok := table.Lookup(item.Model); ok {
			cost := rate.Cost(tokens(item))
			row.CostUSD = &cost
		} else if !slices.Contains(report.unpriced, displayModel(item.Model)) {
			report.unpriced = append(report.unpriced, displayModel(item.Model))
		}
		report.rows = append(report.rows, row)
	}
	slices.Sort(report.unpriced)
	return report, nil
}

func usageRow(item *callstore.Counters) UsageRow {
	row := UsageRow{
		Provider:         item.Provider,
		Account:          item.Account,
		Model:            item.Model,
		Calls:            item.Calls,
		OK:               item.OK,
		RateLimited:      item.RateLimited,
		InputTokens:      item.InputTokens,
		OutputTokens:     item.OutputTokens,
		CacheReadTokens:  item.CacheReadTokens,
		CacheWriteTokens: item.CacheWriteTokens,
	}
	if !item.Start.IsZero() {
		start := item.Start
		row.Start = &start
	}
	return row
}

func tokens(item *callstore.Counters) pricing.Tokens {
	return pricing.Tokens{
		Input:      item.InputTokens,
		Output:     item.OutputTokens,
		CacheRead:  item.CacheReadTokens,
		CacheWrite: item.CacheWriteTokens,
	}
}

func displayModel(model string) string {
	if model == "" {
		return "unknown"
	}
	return model
}

func (server *Server) handleUsageAPI(writer http.ResponseWriter, request *http.Request) {
	query, err := parseUsageQuery(request)
	if err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	report, err := server.usage(request.Context(), query)
	if err != nil {
		writeHistoryError(writer, err)
		return
	}
	rows := report.rows
	if rows == nil {
		rows = []UsageRow{}
	}
	response := map[string]any{
		"window":          query.window,
		"by":              query.by,
		"since":           query.since.UTC(),
		"rows":            rows,
		"unpriced_models": append([]string{}, report.unpriced...),
		"cost_note":       "estimated API-equivalent cost from the prices file; subscriptions are not billed per token",
	}
	if report.pricesErr != nil {
		response["prices_error"] = report.pricesErr.Error()
	}
	writeJSON(writer, http.StatusOK, response)
}

// usageLine is a usage table row, already formatted.
type usageLine struct {
	Period     string
	Account    string
	Model      string
	Calls      string
	Limited    string
	Input      string
	Output     string
	CacheRead  string
	CacheWrite string
	Cost       string
	Total      bool
}

type usageTotals struct {
	calls, limited                       int64
	input, output, cacheRead, cacheWrite int64
	cost                                 float64
	priced                               bool
}

func (totals *usageTotals) add(row UsageRow) {
	totals.calls += row.Calls
	totals.limited += row.RateLimited
	totals.input += row.InputTokens
	totals.output += row.OutputTokens
	totals.cacheRead += row.CacheReadTokens
	totals.cacheWrite += row.CacheWriteTokens
	if row.CostUSD != nil {
		totals.cost += *row.CostUSD
		totals.priced = true
	}
}

func (totals usageTotals) line(period, account, model string) usageLine {
	line := usageLine{
		Period:     period,
		Account:    account,
		Model:      model,
		Calls:      fmt.Sprint(totals.calls),
		Limited:    fmt.Sprint(totals.limited),
		Input:      formatTokens(totals.input),
		Output:     formatTokens(totals.output),
		CacheRead:  formatTokens(totals.cacheRead),
		CacheWrite: formatTokens(totals.cacheWrite),
		Cost:       "-",
	}
	if totals.priced {
		line.Cost = fmt.Sprintf("$%.2f", totals.cost)
	}
	return line
}

type usageView struct {
	Window     string
	Windows    []string
	By         string
	Groupings  []string
	Since      string
	ByModel    []usageLine
	ByPeriod   []usageLine
	Unpriced   []string
	PricesPath string
	PricesErr  string
	Error      string
}

func (server *Server) handleUsagePage(writer http.ResponseWriter, request *http.Request) {
	query, err := parseUsageQuery(request)
	if err != nil {
		http.Redirect(writer, request, "/usage", http.StatusSeeOther)
		return
	}
	view := usageView{Window: query.window, By: query.by, Since: query.since.Format("Jan 2 15:04")}
	for _, window := range usageWindows {
		view.Windows = append(view.Windows, window.name)
	}
	for _, grouping := range usageGroupings {
		view.Groupings = append(view.Groupings, grouping.name)
	}
	byModelQuery := query
	byModelQuery.step = 0
	totals, err := server.usage(request.Context(), byModelQuery)
	if err != nil {
		view.Error = err.Error()
		server.render(writer, "usage.html", view)
		return
	}
	view.Unpriced, view.PricesPath = totals.unpriced, totals.pricesPath
	if totals.pricesErr != nil {
		view.PricesErr = totals.pricesErr.Error()
	}
	var all usageTotals
	for _, row := range totals.rows {
		var one usageTotals
		one.add(row)
		all.add(row)
		view.ByModel = append(view.ByModel, one.line("", row.Provider+"/"+row.Account, displayModel(row.Model)))
	}
	if len(totals.rows) > 1 {
		line := all.line("", "total", "")
		line.Total = true
		view.ByModel = append(view.ByModel, line)
	}

	if query.step > 0 {
		periods, err := server.usage(request.Context(), query)
		if err != nil {
			view.Error = err.Error()
		}
		// Merge models within each period and account, newest period first.
		type periodKey struct {
			start   time.Time
			account string
		}
		merged := map[periodKey]*usageTotals{}
		var order []periodKey
		for _, row := range periods.rows {
			id := periodKey{*row.Start, row.Provider + "/" + row.Account}
			if merged[id] == nil {
				merged[id] = &usageTotals{}
				order = append(order, id)
			}
			merged[id].add(row)
		}
		slices.Reverse(order)
		layout := "Jan 2 15:04"
		if query.step >= 24*time.Hour {
			layout = "Mon Jan 2"
		}
		for _, id := range order {
			view.ByPeriod = append(view.ByPeriod, merged[id].line(id.start.In(query.location).Format(layout), id.account, ""))
		}
	}
	server.render(writer, "usage.html", view)
}

// formatTokens prints a token count compactly, such as 950, 12.3k, or 4.56M.
func formatTokens(count int64) string {
	switch {
	case count >= 1_000_000_000:
		return fmt.Sprintf("%.2fB", float64(count)/1e9)
	case count >= 1_000_000:
		return fmt.Sprintf("%.2fM", float64(count)/1e6)
	case count >= 10_000:
		return fmt.Sprintf("%.1fk", float64(count)/1e3)
	default:
		return fmt.Sprint(count)
	}
}
