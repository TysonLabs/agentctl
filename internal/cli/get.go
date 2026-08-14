package cli

import (
	"context"
	"fmt"

	"github.com/TysonLabs/agentctl/internal/client"
	"github.com/TysonLabs/agentctl/internal/render"
)

func (a *app) cmdGet(args []string) error {
	if len(args) != 2 {
		return usageError("usage: agentctl get <service.env> <path> [--raw]")
	}
	name, rawPath := args[0], args[1]

	reg, err := a.loadRegistry()
	if err != nil {
		return err
	}
	svc, err := a.requireWired(reg, name)
	if err != nil {
		return err
	}
	p, q, err := client.NormalizePath(rawPath)
	if err != nil {
		return usageError(err.Error())
	}
	c, err := a.newClient(svc, a.opts.timeout)
	if err != nil {
		return err
	}
	resp, err := c.Get(context.Background(), p, q)
	if err != nil {
		return err
	}
	a.printBody(resp.Body)
	if resp.Status >= 400 {
		return &httpError{status: resp.Status, msg: fmt.Sprintf("HTTP %d from %s %s", resp.Status, name, p)}
	}
	return nil
}

func (a *app) printBody(body []byte) {
	if a.opts.raw {
		a.stdout.Write(body)
		return
	}
	if pretty, ok := render.PrettyJSON(body); ok {
		a.stdout.Write(pretty)
		return
	}
	a.stdout.Write(render.StripControl(body))
}
