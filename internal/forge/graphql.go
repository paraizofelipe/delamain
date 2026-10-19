package forge

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

// graphql runs a GraphQL query through the REST client, so it shares its auth, retries and
// User-Agent, and decodes the response's data into out. GraphQL errors arrive with a 200.
func (g github) graphql(ctx context.Context, query string, vars map[string]any, out any) error {
	// api.github.com/graphql on github.com, <host>/api/graphql on Enterprise (base <host>/api/v3/).
	endpoint := g.api.BaseURL()
	if strings.HasSuffix(endpoint, "/api/v3/") {
		endpoint = strings.TrimSuffix(endpoint, "v3/")
	}
	body := map[string]any{"query": query}
	if vars != nil {
		body["variables"] = vars
	}
	req, err := g.api.NewRequest(ctx, http.MethodPost, endpoint+"graphql", body)
	if err != nil {
		return g.fail("graphql", err)
	}
	var resp struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if _, err := g.api.Do(req, &resp); err != nil {
		return g.fail("graphql", err)
	}
	if len(resp.Errors) > 0 {
		msgs := make([]string, 0, len(resp.Errors))
		for _, e := range resp.Errors {
			msgs = append(msgs, e.Message)
		}
		return g.fail("graphql", errors.New(strings.Join(msgs, "; ")))
	}
	if err := json.Unmarshal(resp.Data, out); err != nil {
		return g.fail("graphql", err)
	}
	return nil
}
