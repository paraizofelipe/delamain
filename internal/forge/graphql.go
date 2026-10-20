package forge

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	gogithub "github.com/google/go-github/v92/github"
)

// graphql runs a GitHub GraphQL query through go-github, so it shares the token, User-Agent,
// retries and error handling of the REST calls, and decodes the data field into out.
func (g *githubClient) graphql(ctx context.Context, query string, vars map[string]any, out any) error {
	var res struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	err := g.do(ctx, "graphql", func() (*gogithub.Response, error) {
		req, err := g.api.NewRequest(ctx, "POST", "graphql", map[string]any{"query": query, "variables": vars})
		if err != nil {
			return nil, err
		}
		return g.api.Do(req, &res)
	})
	if err != nil {
		return err
	}
	if len(res.Errors) > 0 {
		msgs := make([]string, 0, len(res.Errors))
		for _, e := range res.Errors {
			msgs = append(msgs, e.Message)
		}
		return errors.New("github graphql: " + truncate(redact(strings.Join(msgs, "; "), g.token), maxErrorLen))
	}
	return json.Unmarshal(res.Data, out)
}
