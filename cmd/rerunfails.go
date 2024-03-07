package cmd

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"

	"golang.org/x/tools/cover"
	"gotest.tools/gotestsum/internal/coverprofile"
	"gotest.tools/gotestsum/testjson"
)

type rerunOpts struct {
	runFlag          string
	pkg              string
	coverprofileFlag string
}

func (o rerunOpts) Args() []string {
	var result []string
	if o.runFlag != "" {
		result = append(result, o.runFlag)
	}
	if o.pkg != "" {
		result = append(result, o.pkg)
	}
	if o.coverprofileFlag != "" {
		result = append(result, o.coverprofileFlag)
	}
	return result
}

func (o rerunOpts) withCoverprofile(coverprofile string) rerunOpts {
	o.coverprofileFlag = "-coverprofile=" + coverprofile
	return o
}

func newRerunOptsFromTestCase(tc testjson.TestCase) rerunOpts {
	return rerunOpts{
		runFlag:          goTestRunFlagForTestCase(tc.Test),
		pkg:              tc.Package,
		coverprofileFlag: "",
	}
}

type testCaseFilter func([]testjson.TestCase) []testjson.TestCase

func rerunFailsFilter(o *options) testCaseFilter {
	if o.rerunFailsRunRootCases {
		return func(tcs []testjson.TestCase) []testjson.TestCase {
			var result []testjson.TestCase
			for _, tc := range tcs {
				if !tc.Test.IsSubTest() {
					result = append(result, tc)
				}
			}
			return result
		}
	}
	return testjson.FilterFailedUnique
}

func rerunFailed(ctx context.Context, opts *options, scanConfig testjson.ScanConfig) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	tcFilter := rerunFailsFilter(opts)

	// We need to take special care for the coverprofile file in the rerun
	// failed case. If we pass the same `-coverprofile` flag to the `go test`
	// command, it will overwrite the file. We need to combine the coverprofile
	// files from the original run and the rerun.
	isCoverprofile, mainProfilePath := coverprofile.ParseCoverProfile(opts.args)
	rerunProfiles := []*cover.Profile{}

	rec := newFailureRecorderFromExecution(scanConfig.Execution)
	for attempts := 0; rec.count() > 0 && attempts < opts.rerunFailsMaxAttempts; attempts++ {
		testjson.PrintSummary(opts.stdout, scanConfig.Execution, testjson.SummarizeNone)
		opts.stdout.Write([]byte("\n")) // nolint: errcheck

		nextRec := newFailureRecorder(scanConfig.Handler)
		for i, tc := range tcFilter(rec.failures) {
			rerunOpts := newRerunOptsFromTestCase(tc)
			rerunProfilePath := ""
			if isCoverprofile {
				// create a new unique coverprofile filenames for each rerun
				rerunProfilePath = fmt.Sprintf("%s.%d.%d", mainProfilePath, attempts, i)
				rerunOpts = rerunOpts.withCoverprofile(rerunProfilePath)
			}
			goTestProc, err := startGoTestFn(ctx, "", goTestCmdArgs(opts, rerunOpts))
			if err != nil {
				return err
			}
			cfg := testjson.ScanConfig{
				RunID:     attempts + 1,
				Stdout:    goTestProc.stdout,
				Stderr:    goTestProc.stderr,
				Handler:   nextRec,
				Execution: scanConfig.Execution,
				Stop:      cancel,
			}
			if _, err := testjson.ScanTestOutput(cfg); err != nil {
				return err
			}
			exitErr := goTestProc.cmd.Wait()
			if exitErr != nil {
				nextRec.lastErr = exitErr
			}

			// Need to wait for the go test command to finish before combining
			// the coverprofile files, but before checking for errors.  Even if
			// there is errors, we still need to combine the coverprofile files.
			if isCoverprofile {
				rerunProfile, err := cover.ParseProfiles(rerunProfilePath)
				if err != nil {
					return fmt.Errorf("failed to parse coverprofile %s: %v", rerunProfilePath, err)
				}

				rerunProfiles = append(rerunProfiles, rerunProfile...)

				// Once we read the rerun profiles from files to memory, we can
				// safely delete the rerun profile. This will allow us to avoid
				// extra clean up in the case that we error out in future
				// attempts.
				if err := os.Remove(rerunProfilePath); err != nil {
					return fmt.Errorf("failed to remove coverprofile %s after combined with the main profile: %v", rerunProfilePath, err)
				}
			}

			if err := hasErrors(exitErr, scanConfig.Execution); err != nil {
				return err
			}
		}
		rec = nextRec
	}

	// Write the combined coverprofile files with the main coverprofile file
	if isCoverprofile {
		if err := coverprofile.Combine(mainProfilePath, rerunProfiles); err != nil {
			return fmt.Errorf("failed to combine coverprofiles: %v", err)
		}
	}

	return rec.lastErr
}

// startGoTestFn is a shim for testing
var startGoTestFn = startGoTest

func hasErrors(err error, exec *testjson.Execution) error {
	switch {
	case len(exec.Errors()) > 0:
		return fmt.Errorf("rerun aborted because previous run had errors")
	// Exit code 0 and 1 are expected.
	case ExitCodeWithDefault(err) > 1:
		return fmt.Errorf("unexpected go test exit code: %v", err)
	case exec.HasPanic():
		return fmt.Errorf("rerun aborted because previous run had a suspected panic and some test may not have run")
	default:
		return nil
	}
}

type failureRecorder struct {
	testjson.EventHandler
	failures []testjson.TestCase
	lastErr  error
}

func newFailureRecorder(handler testjson.EventHandler) *failureRecorder {
	return &failureRecorder{EventHandler: handler}
}

func newFailureRecorderFromExecution(exec *testjson.Execution) *failureRecorder {
	return &failureRecorder{failures: exec.Failed()}
}

func (r *failureRecorder) Event(event testjson.TestEvent, execution *testjson.Execution) error {
	if !event.PackageEvent() && event.Action == testjson.ActionFail {
		pkg := execution.Package(event.Package)
		tc := pkg.LastFailedByName(event.Test)
		r.failures = append(r.failures, tc)
	}
	return r.EventHandler.Event(event, execution)
}

func (r *failureRecorder) count() int {
	return len(r.failures)
}

func goTestRunFlagForTestCase(test testjson.TestName) string {
	if test.IsSubTest() {
		parts := strings.Split(string(test), "/")
		var sb strings.Builder
		sb.WriteString("-test.run=")
		for i, p := range parts {
			if i > 0 {
				sb.WriteByte('/')
			}
			sb.WriteByte('^')
			sb.WriteString(regexp.QuoteMeta(p))
			sb.WriteByte('$')
		}
		return sb.String()
	}
	return "-test.run=^" + regexp.QuoteMeta(test.Name()) + "$"
}

func writeRerunFailsReport(opts *options, exec *testjson.Execution) error {
	if opts.rerunFailsMaxAttempts == 0 || opts.rerunFailsReportFile == "" {
		return nil
	}

	type testCaseCounts struct {
		total  int
		failed int
	}

	names := []string{}
	results := map[string]testCaseCounts{}
	for _, failure := range exec.Failed() {
		name := failure.Package + "." + failure.Test.Name()
		if _, ok := results[name]; ok {
			continue
		}
		names = append(names, name)

		pkg := exec.Package(failure.Package)
		counts := testCaseCounts{}

		for _, tc := range pkg.Failed {
			if tc.Test == failure.Test {
				counts.total++
				counts.failed++
			}
		}
		for _, tc := range pkg.Passed {
			if tc.Test == failure.Test {
				counts.total++
			}
		}
		// Skipped tests are not counted, but presumably skipped tests can not fail
		results[name] = counts
	}

	fh, err := os.Create(opts.rerunFailsReportFile)
	if err != nil {
		return err
	}

	sort.Strings(names)
	for _, name := range names {
		counts := results[name]
		fmt.Fprintf(fh, "%s: %d runs, %d failures\n", name, counts.total, counts.failed)
	}
	return nil
}
