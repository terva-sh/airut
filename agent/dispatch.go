package agent

import (
	"context"
	"io"
	"net/http"

	"terva.sh/terva/packages/provider"
)

// dispatchContext carries the per-request selector into the HTTP boundary.
// Provider clients can perform setup and serialization before this point.
type dispatchContext struct{}
type dispatchTransport struct{ inner http.RoundTripper }

// physicalFailure deliberately has no Unwrap: the provider's silent transport
// retry loop must return control to airut, where metadata can be reselected.
// Keep the typed classification, never the raw body/URL/transport error.
type physicalFailure struct{ class string }

func (e *physicalFailure) Error() string { return "agent: inference request failed (" + e.class + ")" }

func (t dispatchTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	guard, _ := req.Context().Value(dispatchContext{}).(func(context.Context) error)
	if guard != nil {
		if err := guard(req.Context()); err != nil {
			return nil, err
		}
	}
	resp, err := t.inner.RoundTrip(req)
	if guard == nil {
		return resp, err
	}
	if err != nil {
		return nil, &physicalFailure{class: classify(context.Background(), err)}
	}
	if resp.StatusCode == 502 || resp.StatusCode == 503 || resp.StatusCode == 504 {
		// These statuses are silently retried by terva's provider. Return a typed
		// failure instead, so the next attempt gets new discovery AND metadata.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		return nil, &physicalFailure{class: class5xx}
	}
	return resp, nil
}

type httpBoundaryClient struct{ provider.Client }

func (c httpBoundaryClient) Unwrap() provider.Client { return c.Client }
func (c httpBoundaryClient) atHTTPBoundary()         {}

// hasHTTPBoundary follows the same Unwrap seam as the provider's metadata and
// HTTP-client configuration. Application decorators must not hide admission.
func hasHTTPBoundary(client provider.Client) bool {
	for client != nil {
		if _, ok := client.(interface{ atHTTPBoundary() }); ok {
			return true
		}
		u, ok := client.(interface{ Unwrap() provider.Client })
		if !ok {
			return false
		}
		client = u.Unwrap()
	}
	return false
}

// guardedProvider is configured before any request. Each candidate gets its
// own policy wrapper over a shared, long-lived per-provider connection pool.
func guardedProvider(client provider.Client, pool *http.Client) provider.Client {
	copy := *pool
	transport := pool.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	copy.Transport = dispatchTransport{inner: transport}
	copy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return httpBoundaryClient{provider.WithHTTPClient(client, &copy)}
}

func withDispatch(ctx context.Context, guard func(context.Context) error) context.Context {
	return context.WithValue(ctx, dispatchContext{}, guard)
}
