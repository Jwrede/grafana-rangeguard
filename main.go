// grafana-rangeguard enforces time range limits on Grafana queries.
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"
)

var version = "dev"

func usage() {
	fmt.Fprint(os.Stderr, `usage:
  grafana-rangeguard -config FILE              run the proxy
  grafana-rangeguard check -config FILE        validate the configuration and print the rules
  grafana-rangeguard explain -config FILE -from now-90d -to now
        [-ds-uid UID] [-ds-type TYPE] [-role ROLE] [-user LOGIN] [-org N]
        [-dashboard UID] [-expr EXPR]          show what the proxy would do with a request
  grafana-rangeguard -version
`)
}

func main() {
	args := os.Args[1:]
	cmd := "serve"
	if len(args) > 0 && (args[0] == "check" || args[0] == "explain" || args[0] == "serve") {
		cmd, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet("grafana-rangeguard", flag.ExitOnError)
	fs.Usage = usage
	path := fs.String("config", "", "configuration file")
	showVersion := fs.Bool("version", false, "print the version")
	from := fs.String("from", "now-6h", "")
	to := fs.String("to", "now", "")
	dsUID := fs.String("ds-uid", "", "")
	dsType := fs.String("ds-type", "", "")
	role := fs.String("role", "", "")
	user := fs.String("user", "", "")
	org := fs.Int64("org", 1, "")
	dash := fs.String("dashboard", "", "")
	expr := fs.String("expr", "", "")
	fs.Parse(args)
	if *showVersion {
		fmt.Println(version)
		return
	}
	if *path == "" {
		usage()
		os.Exit(2)
	}
	cfg, err := loadConfig(*path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	switch cmd {
	case "check":
		printRules(cfg)
	case "explain":
		now := time.Now()
		f, err1 := grafanaTime(*from, now, false)
		t, err2 := grafanaTime(*to, now, true)
		if err1 != nil || err2 != nil {
			fmt.Fprintln(os.Stderr, "cannot parse -from or -to")
			os.Exit(2)
		}
		sub := Subject{Role: *role, User: *user, Org: *org, Dashboard: *dash}
		lim := cfg.limitsFor(sub, DS{UID: *dsUID, Type: *dsType, Known: *dsUID != ""})
		extra, _ := exprReach(*expr)
		d := decide(lim, f, t, now, extra)
		fmt.Printf("rule:      %s (max_range %s, max_lookback %s)\n", lim.Rules[0], orNone(lim.MaxRange), orNone(lim.MaxLookback))
		fmt.Printf("requested: %s\n", fmtDur(t.Sub(f)))
		switch {
		case d.Reject:
			fmt.Printf("decision:  reject\nreason:    %s\n", d.Reason)
			os.Exit(3)
		case d.Changed:
			fmt.Printf("decision:  clamp to %s .. %s\nreason:    %s\n", d.From.UTC().Format(time.RFC3339), d.To.UTC().Format(time.RFC3339), d.Reason)
		default:
			fmt.Println("decision:  pass")
		}
	default:
		srv := NewServer(cfg)
		if cfg.MetricsListen != "" {
			mux := http.NewServeMux()
			mux.HandleFunc("/metrics", srv.metricsHandler)
			go func() { log.Fatal(http.ListenAndServe(cfg.MetricsListen, mux)) }()
		}
		dry := ""
		if cfg.DryRun {
			dry = " (dry run)"
		}
		log.Printf("grafana-rangeguard %s: mode %s, listening on %s, upstream %s, %d rules%s", version, cfg.Mode, cfg.Listen, cfg.Upstream, len(cfg.Rules), dry)
		log.Fatal((&http.Server{Addr: cfg.Listen, Handler: srv, ReadHeaderTimeout: 30 * time.Second}).ListenAndServe())
	}
}

func orNone(d time.Duration) string {
	if d == 0 {
		return "none"
	}
	return fmtDur(d)
}

func printRules(cfg *Config) {
	fmt.Printf("mode %s, upstream %s, %d rules, first match wins\n", cfg.Mode, cfg.Upstream, len(cfg.Rules))
	for i, r := range cfg.Rules {
		fmt.Printf("%2d. %-24s max_range %-6s max_lookback %-6s %-6s %s\n", i+1, r.Name, orNone(r.maxRange), orNone(r.maxLookback), r.Action, r.Match)
	}
	fmt.Printf("    %-24s max_range %-6s max_lookback %-6s %s\n", "defaults", orNone(cfg.Defaults.maxRange), orNone(cfg.Defaults.maxLookback), cfg.Defaults.Action)
}
