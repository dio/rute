package latency

import (
	"fmt"
	"io"
	"math"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
)

// ParseBuckets parses ascending, comma-separated upper bounds.
func ParseBuckets(value string) ([]int, error) {
	var buckets []int
	for part := range strings.SplitSeq(value, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		bound, err := strconv.Atoi(part)
		if err != nil || bound < 1 {
			return nil, fmt.Errorf("invalid bucket %q", part)
		}
		if len(buckets) > 0 && bound <= buckets[len(buckets)-1] {
			return nil, fmt.Errorf("buckets must be ascending: %q", value)
		}
		buckets = append(buckets, bound)
	}
	if len(buckets) == 0 {
		return nil, fmt.Errorf("no buckets in %q", value)
	}
	return buckets, nil
}

// BucketLabel returns the label of the bucket containing value, and the
// bucket's position for ordering.
func BucketLabel(buckets []int, value int) (string, int) {
	lower := 1
	for index, upper := range buckets {
		if value <= upper {
			if lower == upper {
				return strconv.Itoa(upper), index
			}
			return fmt.Sprintf("%d-%d", lower, upper), index
		}
		lower = upper + 1
	}
	return fmt.Sprintf("%d+", lower), len(buckets)
}

// Row summarizes the calls in one group.
type Row struct {
	Group        string
	Bucket       string
	order        int
	Calls        int
	OK           int
	RateLimited  int
	Canceled     int
	Errors       int
	WaitP50      float64
	WaitP90      float64
	TokensPerSec float64
	MeanDuration float64
	OutputTokens int64
	// Wall is the time between the first call start and the last call end.
	Wall time.Duration
}

// Summarize groups records by group(record) and then by bucket(record), which
// returns a label and a sort position.
func Summarize(
	records []Record,
	bucket func(Record) (string, int),
	group func(Record) string,
) []Row {
	type key struct {
		group string
		order int
	}
	type accumulator struct {
		row        Row
		waits      []float64
		rates      []float64
		duration   float64
		start, end time.Time
	}
	groups := map[key]*accumulator{}
	for _, record := range records {
		label, order := bucket(record)
		groupName := group(record)
		id := key{groupName, order}
		item := groups[id]
		if item == nil {
			item = &accumulator{row: Row{Group: groupName, Bucket: label, order: order}}
			groups[id] = item
		}
		item.row.Calls++
		switch record.Outcome {
		case OutcomeOK:
			item.row.OK++
		case OutcomeRateLimited:
			item.row.RateLimited++
		case OutcomeCanceled, OutcomeTimeout:
			item.row.Canceled++
		default:
			item.row.Errors++
		}
		if record.WaitMS > 0 {
			item.waits = append(item.waits, record.WaitMS)
		}
		if record.TokensPerSecond > 0 {
			item.rates = append(item.rates, record.TokensPerSecond)
		}
		item.duration += record.DurationMS
		item.row.OutputTokens += record.OutputTokens
		end := record.Time.Add(time.Duration(record.DurationMS * float64(time.Millisecond)))
		if item.start.IsZero() || record.Time.Before(item.start) {
			item.start = record.Time
		}
		if end.After(item.end) {
			item.end = end
		}
	}
	rows := make([]Row, 0, len(groups))
	for _, item := range groups {
		row := item.row
		row.WaitP50 = percentile(item.waits, 0.5)
		row.WaitP90 = percentile(item.waits, 0.9)
		row.TokensPerSec = percentile(item.rates, 0.5)
		row.MeanDuration = item.duration / float64(row.Calls)
		row.Wall = item.end.Sub(item.start)
		rows = append(rows, row)
	}
	slices.SortFunc(rows, func(left, right Row) int {
		if compared := strings.Compare(left.Group, right.Group); compared != 0 {
			return compared
		}
		return left.order - right.order
	})
	return rows
}

func percentile(values []float64, quantile float64) float64 {
	if len(values) == 0 {
		return math.NaN()
	}
	sorted := slices.Clone(values)
	slices.Sort(sorted)
	index := int(math.Ceil(quantile*float64(len(sorted)))) - 1
	return sorted[min(max(index, 0), len(sorted)-1)]
}

// WriteTable prints rows. bucketTitle names the bucket column. When
// throughput is true, it adds aggregate output tokens per second over each
// group's wall time, which is meaningful only for a bench run.
func WriteTable(writer io.Writer, rows []Row, bucketTitle string, throughput bool) error {
	table := tabwriter.NewWriter(writer, 0, 0, 2, ' ', tabwriter.AlignRight)
	header := "GROUP\t" + bucketTitle + "\tCALLS\tOK\t429\tCANCEL\tERROR\t" +
		"WAIT P50\tWAIT P90\tTOK/S P50\tMEAN CALL"
	if throughput {
		header += "\tTOTAL TOK/S"
	}
	if _, err := fmt.Fprintln(table, header+"\t"); err != nil {
		return err
	}
	for _, row := range rows {
		line := fmt.Sprintf(
			"%s\t%s\t%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s",
			row.Group,
			row.Bucket,
			row.Calls,
			Share(row.OK, row.Calls),
			Share(row.RateLimited, row.Calls),
			Share(row.Canceled, row.Calls),
			Share(row.Errors, row.Calls),
			Seconds(row.WaitP50),
			Seconds(row.WaitP90),
			Rate(row.TokensPerSec),
			Seconds(row.MeanDuration),
		)
		if throughput {
			total := math.NaN()
			if row.Wall > 0 {
				total = float64(row.OutputTokens) / row.Wall.Seconds()
			}
			line += "\t" + Rate(total)
		}
		if _, err := fmt.Fprintln(table, line+"\t"); err != nil {
			return err
		}
	}
	return table.Flush()
}

// Share formats count as a whole percentage of total.
func Share(count, total int) string {
	if total == 0 {
		return "-"
	}
	return fmt.Sprintf("%.0f%%", 100*float64(count)/float64(total))
}

// Seconds formats milliseconds as seconds, or "-" for NaN.
func Seconds(milliseconds float64) string {
	if math.IsNaN(milliseconds) {
		return "-"
	}
	return fmt.Sprintf("%.1fs", milliseconds/1000)
}

// Rate formats a per-second rate, or "-" for NaN.
func Rate(value float64) string {
	if math.IsNaN(value) {
		return "-"
	}
	return fmt.Sprintf("%.0f", value)
}
