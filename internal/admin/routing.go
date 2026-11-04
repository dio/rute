package admin

import (
	"encoding/json"
	"errors"
	"mime"
	"net/http"
	"strconv"
	"strings"

	"github.com/dio/rute/internal/codexpool"
)

// RoutingRequest is the body of PUT /v1/providers/{provider}/routing.
type RoutingRequest struct {
	Balance     string   `json:"balance"`
	Order       []string `json:"order"`
	MaxInFlight int      `json:"max_in_flight"`
}

// applyRouting changes a provider's routing until rute restarts.
func (server *Server) applyRouting(provider string, routing RoutingRequest) (codexpool.Status, error) {
	pool := server.config.Pools[provider]
	if pool == nil {
		return codexpool.Status{}, errUnknownProvider
	}
	mode, err := codexpool.ParseBalance(routing.Balance)
	if err != nil {
		return codexpool.Status{}, err
	}
	var order []string
	for _, name := range routing.Order {
		if name = strings.TrimSpace(name); name != "" {
			order = append(order, name)
		}
	}
	if err := pool.SetRouting(mode, order, routing.MaxInFlight); err != nil {
		return codexpool.Status{}, err
	}
	return pool.Status(), nil
}

var errUnknownProvider = errors.New("unknown provider")

func (server *Server) handleRoutingAPI(writer http.ResponseWriter, request *http.Request) {
	mediaType, _, _ := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if mediaType != "application/json" {
		writeError(writer, http.StatusUnsupportedMediaType, "Content-Type must be application/json")
		return
	}
	var body RoutingRequest
	decoder := json.NewDecoder(http.MaxBytesReader(writer, request.Body, maxFormBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		writeError(writer, http.StatusBadRequest, "invalid JSON request")
		return
	}
	status, err := server.applyRouting(request.PathValue("provider"), body)
	if errors.Is(err, errUnknownProvider) {
		writeError(writer, http.StatusNotFound, err.Error())
		return
	}
	if err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(writer, http.StatusOK, status)
}

type routingView struct {
	CSRF        string
	Provider    string
	Balance     string
	Balances    []codexpool.Balance
	Order       string
	MaxInFlight int
	Accounts    []string
	Error       string
}

func (server *Server) routingPage(provider string) (routingView, bool) {
	pool := server.config.Pools[provider]
	if pool == nil {
		return routingView{}, false
	}
	status := pool.Status()
	view := routingView{
		CSRF:        server.csrf,
		Provider:    provider,
		Balance:     string(status.Balance),
		Balances:    []codexpool.Balance{codexpool.BalanceSpread, codexpool.BalancePriority, codexpool.BalanceFailover},
		Order:       strings.Join(status.Order, ", "),
		MaxInFlight: status.MaxInFlight,
	}
	for _, account := range status.Accounts {
		view.Accounts = append(view.Accounts, account.Name)
	}
	return view, true
}

func (server *Server) handleRoutingPage(writer http.ResponseWriter, request *http.Request) {
	view, ok := server.routingPage(request.PathValue("provider"))
	if !ok {
		http.Redirect(writer, request, "/", http.StatusSeeOther)
		return
	}
	server.render(writer, "routing.html", view)
}

func (server *Server) handleRoutingForm(writer http.ResponseWriter, request *http.Request) {
	request.Body = http.MaxBytesReader(writer, request.Body, maxFormBytes)
	if err := request.ParseForm(); err != nil || !server.checkCSRF(request) {
		writeError(writer, http.StatusForbidden, "invalid form token; reload the page and try again")
		return
	}
	provider := request.PathValue("provider")
	view, ok := server.routingPage(provider)
	if !ok {
		http.Redirect(writer, request, "/", http.StatusSeeOther)
		return
	}
	view.Balance = request.PostFormValue("balance")
	view.Order = request.PostFormValue("order")
	limit, err := strconv.Atoi(strings.TrimSpace(request.PostFormValue("max_in_flight")))
	if err != nil {
		view.Error = "max in flight must be a whole number"
	} else {
		view.MaxInFlight = limit
		_, err = server.applyRouting(provider, RoutingRequest{
			Balance:     view.Balance,
			Order:       strings.Split(view.Order, ","),
			MaxInFlight: limit,
		})
		if err == nil {
			http.Redirect(writer, request, "/", http.StatusSeeOther)
			return
		}
		view.Error = err.Error()
	}
	writer.WriteHeader(http.StatusBadRequest)
	server.render(writer, "routing.html", view)
}
