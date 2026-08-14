package cli

import (
	"context"
	"fmt"

	"github.com/TysonLabs/agentctl/internal/render"
)

func (a *app) cmdEndpoints(args []string) error {
	if len(args) != 1 {
		return usageError("usage: agentctl endpoints <service.env>")
	}
	name := args[0]

	reg, err := a.loadRegistry()
	if err != nil {
		return err
	}
	svc, err := a.requireWired(reg, name)
	if err != nil {
		return err
	}
	c, err := a.newClient(svc, a.opts.timeout)
	if err != nil {
		return err
	}
	resp, err := c.Get(context.Background(), "/agent", "")
	if err != nil {
		return err
	}
	if resp.Status >= 400 {
		a.printBody(resp.Body)
		return &httpError{status: resp.Status, msg: fmt.Sprintf("HTTP %d from %s /agent", resp.Status, name)}
	}
	if eps, ok := render.ParseEndpoints(resp.Body); ok {
		render.EndpointsTable(a.stdout, eps)
		return nil
	}
	a.errf("index from %s does not follow the endpoints convention; showing raw JSON", name)
	a.printBody(resp.Body)
	return nil
}
