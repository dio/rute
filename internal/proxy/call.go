package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/dio/rute/internal/codexpool"
	"github.com/dio/rute/internal/latency"
)

// upstreamCall holds the account lease for one downstream inference call and
// records its latency when the upstream body is closed.
type upstreamCall struct {
	ctx         context.Context
	credentials CredentialSource
	metrics     latency.Sink
	start       time.Time
	record      latency.Record
	lease       *codexpool.Lease
}

func (handler *Handler) newCall(
	ctx context.Context,
	provider string,
	endpoint string,
	model string,
) *upstreamCall {
	return &upstreamCall{
		ctx:         ctx,
		credentials: handler.credentials[provider],
		metrics:     handler.config.Metrics,
		start:       time.Now(),
		record: latency.Record{
			Source:   "proxy",
			Provider: provider,
			Endpoint: endpoint,
			Model:    model,
		},
	}
}

// acquired records a new lease for an upstream attempt.
func (call *upstreamCall) acquired(lease codexpool.Lease) {
	call.lease = &lease
	call.record.Account = lease.AccountName
	call.record.InFlight = lease.InFlight
	call.record.Attempts++
}

// retry releases the lease of an attempt that will not be returned.
func (call *upstreamCall) retry() {
	call.release()
}

func (call *upstreamCall) release() {
	if call.lease == nil {
		return
	}
	call.credentials.Release(call.lease.AccountID)
	call.lease = nil
}

// fail releases the lease and records a call that produced no response.
func (call *upstreamCall) fail(err error) {
	call.release()
	// No account was ever selected, so there is no per-account call to record.
	if call.record.Account == "" {
		return
	}
	var unavailable *codexpool.UnavailableError
	switch {
	case call.ctx.Err() != nil:
		call.record.Outcome = latency.OutcomeCanceled
	case errors.As(err, &unavailable) && unavailable.RateLimited:
		call.record.Outcome = latency.OutcomeRateLimited
		call.record.Status = http.StatusTooManyRequests
	default:
		call.record.Outcome = latency.OutcomeError
	}
	call.write(time.Now(), nil)
}

// wrap makes response.Body release the lease and record the call on Close.
func (call *upstreamCall) wrap(response *http.Response) *http.Response {
	call.record.Status = response.StatusCode
	response.Body = &observedBody{
		ReadCloser: response.Body,
		call:       call,
		observer:   latency.NewObserver(),
	}
	return response
}

func (call *upstreamCall) write(end time.Time, observer *latency.Observer) {
	if call.metrics == nil {
		return
	}
	call.record.Time = call.start.UTC()
	call.record.Fill(call.start, end, observer)
	_ = call.metrics.Write(call.record)
}

type observedBody struct {
	io.ReadCloser
	call     *upstreamCall
	observer *latency.Observer
	readErr  error
	once     sync.Once
}

func (body *observedBody) Read(buffer []byte) (int, error) {
	count, err := body.ReadCloser.Read(buffer)
	if count > 0 {
		_, _ = body.observer.Write(buffer[:count])
	}
	if err != nil && body.readErr == nil {
		body.readErr = err
	}
	return count, err
}

func (body *observedBody) Close() error {
	err := body.ReadCloser.Close()
	body.once.Do(func() {
		end := time.Now()
		call := body.call
		call.release()
		body.observer.Finish()
		status := call.record.Status
		switch {
		case status == http.StatusTooManyRequests:
			call.record.Outcome = latency.OutcomeRateLimited
		case status < 200 || status >= 300:
			call.record.Outcome = latency.OutcomeUpstreamError
		case body.observer.Failed():
			call.record.Outcome = latency.OutcomeUpstreamError
		case body.observer.Completed():
			call.record.Outcome = latency.OutcomeOK
		case call.ctx.Err() != nil:
			call.record.Outcome = latency.OutcomeCanceled
		default:
			call.record.Outcome = latency.OutcomeIncomplete
		}
		observer := body.observer
		if status < 200 || status >= 300 {
			// An error body carries no generated tokens to time.
			observer = nil
		}
		call.write(end, observer)
	})
	return err
}
