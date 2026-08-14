package cli

import "github.com/TysonLabs/agentctl/internal/render"

func (a *app) cmdLs(args []string) error {
	if len(args) != 0 {
		return usageError("ls takes no arguments")
	}
	reg, err := a.loadRegistry()
	if err != nil {
		return err
	}
	rows := make([]render.LsRow, 0, len(reg.Services))
	for _, s := range reg.Services {
		rows = append(rows, render.LsRow{
			Name:    s.FullName(),
			Wired:   s.Wired,
			Reason:  s.NotWiredReason,
			BaseURL: s.BaseURL,
			Meta:    s.Meta,
		})
	}
	render.LsTable(a.stdout, rows)
	return nil
}
