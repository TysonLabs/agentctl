package cli

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/TysonLabs/agentctl/internal/client"
	"github.com/TysonLabs/agentctl/internal/registry"
	"github.com/TysonLabs/agentctl/internal/render"
)

const statusDefaultTimeout = 8 * time.Second

type statusResult struct {
	name string
	line string
	code int // 0 ok, 2 http fail, 3 transport fail
}

func (a *app) cmdStatus(args []string) error {
	reg, err := a.loadRegistry()
	if err != nil {
		return err
	}

	var targets []registry.Service
	var skips []statusResult
	if len(args) > 0 {
		for _, name := range args {
			svc, err := a.requireWired(reg, name)
			if err != nil {
				return err // explicitly naming a not-wired/unknown service: exit 1 before any HTTP
			}
			targets = append(targets, svc)
		}
	} else {
		for _, svc := range reg.Services {
			if svc.Wired {
				targets = append(targets, svc)
			} else {
				skips = append(skips, statusResult{
					name: svc.FullName(),
					line: fmt.Sprintf("%s\tSKIP\tnot wired (%s)", svc.FullName(), svc.NotWiredReason),
				})
			}
		}
	}

	results := make([]statusResult, len(targets))
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for i, svc := range targets {
		wg.Add(1)
		go func(i int, svc registry.Service) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i] = a.probe(svc)
		}(i, svc)
	}
	wg.Wait()

	results = append(results, skips...)
	sort.Slice(results, func(i, j int) bool { return results[i].name < results[j].name })

	tw := tabwriter.NewWriter(a.stdout, 0, 4, 2, ' ', 0)
	worst := 0
	for _, r := range results {
		fmt.Fprintln(tw, r.line)
		switch {
		case r.code == 3:
			worst = 3
		case r.code == 2 && worst != 3:
			worst = 2
		}
	}
	tw.Flush()
	switch worst {
	case 3:
		return &client.TransportError{Err: fmt.Errorf("one or more services unreachable")}
	case 2:
		return &httpError{status: worst, msg: "one or more services returned HTTP errors"}
	}
	return nil
}

func (a *app) probe(svc registry.Service) statusResult {
	name := svc.FullName()
	c, err := a.newClient(svc, statusDefaultTimeout)
	if err != nil {
		return statusResult{name: name, line: fmt.Sprintf("%s\tFAIL\t%v", name, err), code: 3}
	}
	start := time.Now()
	ctx := context.Background()

	vresp, err := c.Get(ctx, "/agent/version", "")
	if err != nil {
		return statusResult{name: name, line: fmt.Sprintf("%s\tFAIL\t%v", name, err), code: 3}
	}
	hresp, err := c.Get(ctx, "/agent/health", "")
	if err != nil {
		return statusResult{name: name, line: fmt.Sprintf("%s\tFAIL\t%v", name, err), code: 3}
	}
	dur := time.Since(start).Round(time.Millisecond)

	if vresp.Status >= 400 || hresp.Status >= 400 {
		which, st := "/agent/version", vresp.Status
		if vresp.Status < 400 {
			which, st = "/agent/health", hresp.Status
		}
		return statusResult{name: name, line: fmt.Sprintf("%s\tFAIL\tHTTP %d on %s\t%s", name, st, which, dur), code: 2}
	}
	ver := render.VersionFromBody(vresp.Body)
	return statusResult{name: name, line: fmt.Sprintf("%s\tok\tversion=%s health=ok\t%s", name, ver, dur), code: 0}
}
