// Command muse-budget runs Muse Code under a hard budget from the terminal,
// with a live meter: every model call, what it cost, and what is left.
//
//	muse-budget run --budget 0.50 "fix the failing test in ./billing"
//	muse-budget quote "add refunds to the Payme integration"
//	muse-budget models
//	muse-budget login
//
// It uses an in-memory ledger funded with the budget, so it shows exactly
// what agnts enforces for every workspace, on one machine.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	muse "github.com/rustambekabdusalomov/agnts-muse"
	"github.com/rustambekabdusalomov/agnts-muse/budget"
	"github.com/rustambekabdusalomov/agnts-muse/budget/memledger"
	"github.com/rustambekabdusalomov/agnts-muse/credential"
	"github.com/rustambekabdusalomov/agnts-muse/model"
	"github.com/rustambekabdusalomov/agnts-muse/money"
	"github.com/rustambekabdusalomov/agnts-muse/quote"
)

const usage = `muse-budget runs Meta's Muse Code under a hard, live-enforced budget.

Usage:
  muse-budget run [flags] "task"     run a task (flags: --budget, --model, --dir, --effort, --quiet)
  muse-budget quote [flags] "task"   estimate a task's cost before running it
  muse-budget models                 list Muse models and their prices
  muse-budget login                  sign in with your Meta account (device code)

Muse uses META_API_KEY when set, else the login from 'muse login' or 'muse-budget login'.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	var err error
	switch os.Args[1] {
	case "run":
		err = run(ctx, os.Args[2:])
	case "quote":
		err = quoteCmd(ctx, os.Args[2:])
	case "models":
		err = models()
	case "login":
		err = login(ctx)
	case "-h", "--help", "help":
		fmt.Print(usage)
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "muse-budget:", err)
		os.Exit(1)
	}
}

// Terminal styling, off when stdout is not a terminal or NO_COLOR is set.
var color = func() bool {
	fi, err := os.Stdout.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0 && os.Getenv("NO_COLOR") == ""
}()

func paint(code, s string) string {
	if !color {
		return s
	}
	return "\033[" + code + "m" + s + "\033[0m"
}

func dim(s string) string    { return paint("2", s) }
func bold(s string) string   { return paint("1", s) }
func green(s string) string  { return paint("32", s) }
func yellow(s string) string { return paint("33", s) }
func red(s string) string    { return paint("31", s) }
func cyan(s string) string   { return paint("36", s) }

func bar(spent, limit money.USD, width int) string {
	if limit <= 0 {
		return ""
	}
	n := min(int(int64(spent)*int64(width)/int64(limit)), width)
	b := strings.Repeat("█", n) + strings.Repeat("░", width-n)
	switch {
	case spent*10 >= limit*8:
		return red(b)
	case spent*2 >= limit:
		return yellow(b)
	}
	return green(b)
}

func executor() muse.LocalExecutor { return muse.LocalExecutor{InheritEnv: true} }

func run(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	limitS := fs.String("budget", "0.50", "the most the run may spend, in dollars")
	modelS := fs.String("model", "spark", "model: spark, wonder or a full id")
	dir := fs.String("dir", ".", "the workspace Muse works in")
	effort := fs.String("effort", "", "reasoning effort: minimal … max")
	quiet := fs.Bool("quiet", false, "show only the meter, not Muse's messages and tools")
	_ = fs.Parse(args)
	task := strings.Join(fs.Args(), " ")
	if task == "" {
		return errors.New(`give a task: muse-budget run "fix the failing test"`)
	}
	limit, err := money.Parse(*limitS)
	if err != nil || limit <= 0 {
		return fmt.Errorf("--budget %q: give a positive dollar amount", *limitS)
	}
	m, err := model.Default.Resolve(*modelS)
	if err != nil {
		return err
	}
	workdir, err := filepath.Abs(*dir)
	if err != nil {
		return err
	}

	ledger := memledger.New()
	if _, err := ledger.Deposit(ctx, "you", limit, "budget"); err != nil {
		return err
	}
	eng := muse.New(muse.Config{Ledger: ledger})
	fmt.Printf("%s %s on %s · budget %s · %s\n\n", bold("muse-budget"), muse.Version, cyan(string(m.ID)), bold(limit.String()), dim(workdir))

	start := time.Now()
	res, err := eng.Run(ctx, muse.Request{Prompt: task, Workdir: workdir, Model: m.ID, Effort: *effort, AnnounceBudget: true,
		Budget: &muse.Budget{Key: fmt.Sprint("cli-", start.UnixNano()), Payers: []budget.AccountID{"you"}, Limit: limit}},
		executor(), func(e muse.Event) { show(e, limit, *quiet) })

	fmt.Println()
	switch {
	case err == nil:
		fmt.Println(green("✔ done"))
	case errors.Is(err, muse.ErrRunLimit), errors.Is(err, muse.ErrBudget):
		fmt.Println(yellow("■ stopped at the budget: ") + err.Error())
	default:
		fmt.Println(red("✘ ") + err.Error())
	}
	if res.Text != "" {
		fmt.Println("\n" + res.Text)
	}
	fmt.Printf("\n%s %d calls · %s in (%s cached) · %s out · %s\n", dim("usage"), res.Usage.Calls,
		tokens(res.Usage.Usage.Input), tokens(res.Usage.Usage.Cached), tokens(res.Usage.Usage.Output), time.Since(start).Round(time.Second))
	if r := res.Receipt; r != nil {
		fmt.Printf("%s charged %s · released %s", dim("receipt"), bold(r.Charged.String()), r.Released)
		if r.Unbilled > 0 {
			fmt.Printf(" · unbilled %s", r.Unbilled)
		}
		fmt.Println()
	}
	if err != nil && !errors.Is(err, muse.ErrRunLimit) && !errors.Is(err, muse.ErrBudget) {
		os.Exit(1)
	}
	return nil
}

func show(e muse.Event, limit money.USD, quiet bool) {
	switch e.Kind {
	case muse.EventUsage:
		c := e.Charge
		est := ""
		if c.Estimated {
			est = yellow(" (estimated price)")
		}
		fmt.Printf("  %s call %-3d %s %-10s %s %s / %s%s\n", dim("$"), c.Calls, bar(c.Total, limit, 24), "+"+c.Cost.String(),
			dim("total"), bold(c.Total.String()), limit, est)
	case muse.EventToolStart:
		if !quiet {
			fmt.Printf("  %s %s %s\n", cyan("▸"), e.Tool, dim(oneLine(e.Args, 90)))
		}
	case muse.EventToolEnd:
		if !quiet && e.Failed {
			fmt.Printf("    %s %s\n", red("✘"), dim(oneLine(e.Output, 100)))
		}
	case muse.EventMessage:
		if !quiet && !e.Final {
			fmt.Printf("  %s %s\n", dim("…"), oneLine(e.Text, 110))
		}
	case muse.EventSteer:
		fmt.Printf("  %s %s\n", yellow("⚑ steer"), "told Muse the budget is running low; it should wrap up")
	case muse.EventEscalate:
		fmt.Printf("  %s %s %s\n", cyan("⇧ escalate"), e.Model, dim("("+e.Reason+")"))
	case muse.EventApproval:
		if a := e.Approval; a != nil && !a.Allow {
			fmt.Printf("  %s %s %s\n", red("⛔ denied"), oneLine(firstNonEmpty(a.Command, a.Path, a.Tool), 60), dim("("+a.Rule+")"))
		}
	case muse.EventInterrupt:
		fmt.Printf("  %s %s\n", red("■ interrupt"), e.Reason)
	case muse.EventRetry:
		fmt.Printf("  %s %s\n", dim("↻ retry"), dim(e.Reason))
	case muse.EventWarning:
		fmt.Printf("  %s %s\n", yellow("!"), e.Reason)
	}
}

func quoteCmd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("quote", flag.ExitOnError)
	dir := fs.String("dir", ".", "the workspace to read")
	_ = fs.Parse(args)
	task := strings.Join(fs.Args(), " ")
	if task == "" {
		return errors.New(`give a task: muse-budget quote "add refunds"`)
	}
	workdir, err := filepath.Abs(*dir)
	if err != nil {
		return err
	}
	fmt.Println(dim("Muse is reading the code to size the task…"))
	q, err := quote.Estimate(ctx, muse.New(muse.Config{}), executor(), quote.Request{Task: task, Workdir: workdir})
	if err != nil {
		return err
	}
	kind := "simple"
	if q.Complexity != 0 {
		kind = "complex"
	}
	fmt.Printf("\n%s %s on %s · %d–%d model calls (expected %d)\n", bold("Quote"), kind, cyan(string(q.Model)), q.Calls.Low, q.Calls.High, q.Calls.Expected)
	fmt.Printf("  %s %s   %s %s   %s %s\n", dim("low"), q.Low, dim("expected"), bold(q.Expected.String()), dim("high"), q.High)
	for _, r := range q.Reasons {
		fmt.Println("  · " + r)
	}
	if len(q.Files) > 0 {
		fmt.Println(dim("  files: " + strings.Join(q.Files, ", ")))
	}
	fmt.Printf("%s %s\n", dim("the quote itself cost"), q.Cost)
	return nil
}

func models() error {
	fmt.Printf("%-22s %-7s %10s %10s %10s\n", "model", "tier", "input/M", "cached/M", "output/M")
	for _, m := range model.Default.Models() {
		fmt.Printf("%-22s %-7s %10s %10s %10s\n", m.ID, m.Tier, money.USD(m.Price.Input), money.USD(m.Price.Cached), money.USD(m.Price.Output))
	}
	return nil
}

func login(ctx context.Context) error {
	var f credential.DeviceFlow
	a, err := f.Start(ctx)
	if err != nil {
		return err
	}
	page := firstNonEmpty(a.VerificationURIComplete, a.VerificationURI)
	fmt.Printf("Open %s\nand confirm the code %s\n\nWaiting for approval…\n", bold(page), bold(a.UserCode))
	tok, err := f.Wait(ctx, a)
	if err != nil {
		return err
	}
	c, err := credential.Login(tok, "")
	if err != nil {
		return err
	}
	data, err := c.AuthFile()
	if err != nil {
		return err
	}
	home, err := os.UserConfigDir()
	if err != nil {
		return err
	}
	path := filepath.Join(home, "agnts-muse", "auth.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return err
	}
	fmt.Printf("Signed in. Saved to %s; run Muse with MUSE_AUTH_PATH=%s\n", path, path)
	return nil
}

func tokens(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1000:
		return fmt.Sprintf("%.1fk", float64(n)/1e3)
	}
	return fmt.Sprint(n)
}

func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}
