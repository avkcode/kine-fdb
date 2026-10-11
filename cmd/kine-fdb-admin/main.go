// Copyright 2026 The kine-fdb Authors
// SPDX-License-Identifier: Apache-2.0
//
// kine-fdb-admin: maintenance commands for kine-fdb namespaces — listing,
// statistics, compaction, consistency checks, and deletion.

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/urfave/cli/v2"

	"github.com/avkcode/kine-fdb/pkg/fdbdriver"
	"github.com/avkcode/kine-fdb/pkg/version"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := newApp().RunContext(ctx, os.Args); err != nil {
		fmt.Fprintf(os.Stderr, "kine-fdb-admin: %v\n", err)
		var exit cli.ExitCoder
		if errors.As(err, &exit) {
			os.Exit(exit.ExitCode())
		}
		os.Exit(1)
	}
}

func newApp() *cli.App {
	namespaceFlag := &cli.StringFlag{
		Name:  "namespace",
		Usage: "namespace to operate on (default: the endpoint's namespace)",
	}
	return &cli.App{
		Name:    "kine-fdb-admin",
		Usage:   "inspect and maintain kine-fdb namespaces",
		Version: version.String(),
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "endpoint",
				Usage:   "FoundationDB endpoint, as passed to kine-fdb",
				Value:   "fdb://",
				EnvVars: []string{"KINE_ENDPOINT"},
			},
			&cli.DurationFlag{
				Name:  "timeout",
				Usage: "overall time limit for the command",
				Value: 30 * time.Minute,
			},
		},
		Commands: []*cli.Command{
			{
				Name:  "namespaces",
				Usage: "list namespaces in the cluster",
				Action: func(c *cli.Context) error {
					return withAdmin(c, func(ctx context.Context, a *fdbdriver.Admin) error {
						names, err := a.Namespaces(ctx)
						if err != nil {
							return err
						}
						for _, n := range names {
							fmt.Println(n)
						}
						return nil
					})
				},
			},
			{
				Name:  "stats",
				Usage: "print revision and size statistics as JSON",
				Flags: []cli.Flag{namespaceFlag},
				Action: func(c *cli.Context) error {
					return withAdmin(c, func(ctx context.Context, a *fdbdriver.Admin) error {
						st, err := a.Stats(ctx, c.String("namespace"))
						if err != nil {
							return err
						}
						return printJSON(st)
					})
				},
			},
			{
				Name:  "compact",
				Usage: "compact a namespace and delete unreachable history",
				Flags: []cli.Flag{
					namespaceFlag,
					&cli.Int64Flag{Name: "revision", Usage: "compact up to this revision"},
					&cli.Int64Flag{Name: "retain", Usage: "keep this many most recent revisions"},
				},
				Action: func(c *cli.Context) error {
					rev, retain := c.Int64("revision"), c.Int64("retain")
					if (rev > 0) == (retain > 0) {
						return cli.Exit("compact: specify exactly one of --revision or --retain", 2)
					}
					return withAdmin(c, func(ctx context.Context, a *fdbdriver.Admin) error {
						st, err := a.Compact(ctx, c.String("namespace"), rev, retain)
						if err != nil {
							return err
						}
						return printJSON(st)
					})
				},
			},
			{
				Name:  "verify",
				Usage: "check the internal consistency of a namespace (exit status 3 on problems)",
				Flags: []cli.Flag{namespaceFlag},
				Action: func(c *cli.Context) error {
					return withAdmin(c, func(ctx context.Context, a *fdbdriver.Admin) error {
						rep, err := a.Verify(ctx, c.String("namespace"))
						if err != nil {
							return err
						}
						if err := printJSON(rep); err != nil {
							return err
						}
						if !rep.OK() {
							return cli.Exit(fmt.Sprintf("verify: %d problem(s) found", len(rep.Problems)), 3)
						}
						return nil
					})
				},
			},
			{
				Name:  "delete",
				Usage: "permanently delete a namespace and all of its data",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "namespace", Usage: "namespace to delete", Required: true},
					&cli.BoolFlag{Name: "yes", Usage: "confirm the deletion"},
				},
				Action: func(c *cli.Context) error {
					if !c.Bool("yes") {
						return cli.Exit("delete: this permanently removes all data in the namespace; pass --yes to confirm", 2)
					}
					return withAdmin(c, func(ctx context.Context, a *fdbdriver.Admin) error {
						if err := a.Delete(ctx, c.String("namespace")); err != nil {
							return err
						}
						fmt.Printf("deleted namespace %s\n", c.String("namespace"))
						return nil
					})
				},
			},
		},
	}
}

func withAdmin(c *cli.Context, fn func(context.Context, *fdbdriver.Admin) error) error {
	a, err := fdbdriver.OpenAdmin(c.String("endpoint"))
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(c.Context, c.Duration("timeout"))
	defer cancel()
	return fn(ctx, a)
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
