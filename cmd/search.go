package cmd

import (
	"context"
	"fmt"
	"github.com/humio/cli/api"
	"github.com/olekukonko/tablewriter"
	"github.com/schollz/progressbar/v2"
	"github.com/spf13/cobra"
	"io"
	"os"
	"os/signal"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"
)

func newSearchCmd() *cobra.Command {
	var (
		start      string
		end        string
		live       bool
		fmtStr     string
		noProgress bool
	)

	cmd := &cobra.Command{
		Use:   "search <repo> <query>",
		Short: "Search",
		Args:  cobra.ExactArgs(2),
		Run: func(cmd *cobra.Command, args []string) {
			repository := args[0]
			queryString := args[1]
			client := NewApiClient(cmd)

			ctx := contextCancelledOnInterrupt(context.Background())

			// run in lambda func to be able to defer and delete the query job
			err := func() error {
				var progress queryResultProgressBar
				if !noProgress {
					progress = newQueryResultProgressBar()
				}

				id, err := client.QueryJobs().Create(repository, api.Query{
					QueryString: queryString,
					Start:       start,
					End:         end,
					Live:        live,
				})

				if err != nil {
					return err
				}

				defer func(id string) {
					// Humio will eventually delete the query when we stop polling and we can't do much about errors here.
					_ = client.QueryJobs().Delete(repository, id)
				}(id)

				var result api.QueryResult
				poller := queryJobPoller{
					queryJobs:  client.QueryJobs(),
					repository: repository,
					id:         id,
				}
				result, err = poller.WaitAndPollContext(ctx)

				if err != nil {
					return err
				}

				var printer interface {
					print(api.QueryResult)
				}

				if result.Metadata.IsAggregate {
					printer = newAggregatePrinter(cmd.OutOrStdout())
				} else {
					printer = newEventListPrinter(cmd.OutOrStdout(), fmtStr)
				}


				for !result.Done {
					progress.Update(result)
					result, err = poller.WaitAndPollContext(ctx)
					if err != nil {
						return err
					}
				}

				progress.Finish()

				printer.print(result)

				if live {
					for {
						result, err = poller.WaitAndPollContext(ctx)
						if err != nil {
							return err
						}

						printer.print(result)
					}
				}

				return nil
			}()

			if err == context.Canceled {
				err = nil
			}

			exitOnError(cmd, err, "error running search")
		},
	}

	cmd.Flags().StringVarP(&start, "start", "s", "10m", "Query start time [default 10m]")
	cmd.Flags().StringVarP(&end, "end", "e", "", "Query end time")
	cmd.Flags().BoolVarP(&live, "live", "l", false, "Run a live search and keep outputting until interrupted.")
	cmd.Flags().StringVarP(&fmtStr, "fmt", "f", "{@timestamp} {@rawstring}", "Format string if the result is an event list\n"+
		"Insert fields by wrapping field names in brackets, e.g. {@timestamp} [default: '{@timestamp} {@rawstring}']\n"+
		"Limited format modifiers are supported such as {@timestamp:40} which will right align and left pad @timestamp to 40 characters.\n"+
		"{@timestamp:-40} left aligns and right pads to 40 characters.")
	cmd.Flags().BoolVar(&noProgress, "no-progress", false, "Do not should progress information.")

	return cmd
}

func contextCancelledOnInterrupt(ctx context.Context) context.Context {
	ctx, cancel := context.WithCancel(ctx)

	sigC := make(chan os.Signal, 1)
	signal.Notify(sigC, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigC
		cancel()
	}()

	return ctx
}

type queryResultProgressBar struct {
	bar *progressbar.ProgressBar
}

func newQueryResultProgressBar() queryResultProgressBar {
	b := queryResultProgressBar{
		bar: progressbar.NewOptions(
			0,
			progressbar.OptionSetPredictTime(false),
			progressbar.OptionSetDescription("Searching..."),
			progressbar.OptionClearOnFinish(),
		),
	}
	return b
}

func (b queryResultProgressBar) Update(result api.QueryResult) {
	if b.bar == nil {
		return
	}

	if result.Metadata.TotalWork > 0 {
		b.bar.ChangeMax64(int64(result.Metadata.TotalWork))
		b.bar.Set64(int64(result.Metadata.WorkDone))
	}
}

func (b queryResultProgressBar) Finish() {
	if b.bar == nil {
		return
	}

	b.bar.Finish()
}

type queryJobPoller struct {
	queryJobs  *api.QueryJobs
	repository string
	id         string
	nextPoll   time.Time
}

func (q *queryJobPoller) WaitAndPollContext(ctx context.Context) (api.QueryResult, error) {
	select {
	case <-time.After(q.nextPoll.Sub(time.Now())):
	case <-ctx.Done():
		return api.QueryResult{}, ctx.Err()
	}

	result, err := q.queryJobs.PollContext(ctx, q.repository, q.id)
	if err != nil {
		return result, err
	}

	q.nextPoll = time.Now().Add(time.Duration(result.Metadata.PollAfter) * time.Millisecond)

	return result, err
}

var fieldPrinters = map[string]func(v interface{}) (string, bool){
	"@timestamp": func(v interface{}) (string, bool) {
		fv, ok := v.(float64)
		if !ok {
			return "", false
		}

		sec, msec := int64(fv)/1000, int64(fv)%1000

		t := time.Unix(sec, msec*1000000)

		return t.Format(time.RFC3339Nano), true
	},
}

type eventListPrinter struct {
	printedIds     map[string]bool
	printFields    []string
	w              io.Writer
	printEventFunc func(io.Writer, map[string]interface{})
	fmt            string
}

func newEventListPrinter(w io.Writer, fmt string) *eventListPrinter {
	e := &eventListPrinter{
		printedIds: map[string]bool{},
		w:          w,
	}

	re := regexp.MustCompile(`(\{[^\}]+\})`)
	e.fmt = re.ReplaceAllStringFunc(fmt, func(f string) string {
		field := f[1 : len(f)-1]
		arg := ""

		if strings.Contains(field, ":") {
			pieces := strings.SplitN(field, ":", 2)
			field, arg = pieces[0], pieces[1]
		}

		e.printFields = append(e.printFields, field)
		return "%" + arg + "s"
	})

	e.initPrintFunc()
	return e
}

func (p *eventListPrinter) initPrintFunc() {
	var printers []func(map[string]interface{}) string
	for _, f := range p.printFields {
		f := f
		if printer, hasPrinter := fieldPrinters[f]; hasPrinter {
			printers = append(printers, func(m map[string]interface{}) string {
				v := m[f]
				if str, ok := printer(v); ok {
					return str
				} else {
					return fmt.Sprint(v)
				}
			})
		} else {
			printers = append(printers, func(m map[string]interface{}) string {
				v := m[f]
				return fmt.Sprint(v)
			})
		}
	}

	p.printEventFunc = func(w io.Writer, m map[string]interface{}) {
		fmtArgs := make([]interface{}, len(printers))
		for i, printer := range printers {
			fmtArgs[i] = printer(m)
		}
		fmt.Fprintf(w, p.fmt+"\n", fmtArgs...)
	}
}

func (p *eventListPrinter) print(result api.QueryResult) {
	sort.Slice(result.Events, func(i, j int) bool {
		tsI, hasTsI := result.Events[i]["@timestamp"].(float64)
		tsJ, hasTsJ := result.Events[j]["@timestamp"].(float64)

		switch {
		case hasTsI && hasTsJ:
			return tsI < tsJ
		case !hasTsJ:
			return false
		case !hasTsI:
			return true
		default:
			return false
		}
	})

	for _, e := range result.Events {
		id, hasID := e["@id"].(string)
		if hasID && !p.printedIds[id] {
			p.printEventFunc(p.w, e)
			p.printedIds[id] = true
		} else if !hasID {
			p.printEventFunc(p.w, e)
		}
	}
}

type aggregatePrinter struct {
	w       io.Writer
	columns []string
}

func newAggregatePrinter(w io.Writer) *aggregatePrinter {
	return &aggregatePrinter{
		w: w,
	}
}

func (p *aggregatePrinter) print(result api.QueryResult) {
	f := p.columns
	m := map[string]bool{}
	for _, e := range result.Events {
		for k := range e {
			if !m[k] {
				f = append(f, k)
				m[k] = true
			}
		}
	}
	p.columns = f

	if len(p.columns) == 0 {
		return
	}

	if len(p.columns) == 1 && len(result.Events) == 1 {
		// single column, single result, just print it
		fmt.Fprintln(p.w, result.Events[0][p.columns[0]])
		return
	}

	t := tablewriter.NewWriter(p.w)
	t.SetAutoFormatHeaders(false)
	t.SetBorder(false)
	t.SetHeader(p.columns)
	t.SetHeaderLine(false)

	for _, e := range result.Events {
		var r []string
		for _, i := range p.columns {
			v, hasField := e[i]
			if hasField {
				r = append(r, fmt.Sprint(v))
			} else {
				r = append(r, "")
			}
		}
		t.Append(r)
	}

	t.Render()
	fmt.Fprintln(p.w)
}
