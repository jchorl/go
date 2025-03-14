// Copyright 2020 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package testing

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sync/atomic"
	"time"
)

func initFuzzFlags() {
	matchFuzz = flag.String("test.fuzz", "", "run the fuzz target matching `regexp`")
	flag.Var(&fuzzDuration, "test.fuzztime", "time to spend fuzzing; default is to run indefinitely")
	flag.Var(&minimizeDuration, "test.fuzzminimizetime", "time to spend minimizing a value after finding a crash")
	fuzzCacheDir = flag.String("test.fuzzcachedir", "", "directory where interesting fuzzing inputs are stored")
	isFuzzWorker = flag.Bool("test.fuzzworker", false, "coordinate with the parent process to fuzz random values")
}

var (
	matchFuzz        *string
	fuzzDuration     durationOrCountFlag
	minimizeDuration = durationOrCountFlag{d: 60 * time.Second, allowZero: true}
	fuzzCacheDir     *string
	isFuzzWorker     *bool

	// corpusDir is the parent directory of the target's seed corpus within
	// the package.
	corpusDir = "testdata/fuzz"
)

// fuzzWorkerExitCode is used as an exit code by fuzz worker processes after an internal error.
// This distinguishes internal errors from uncontrolled panics and other crashes.
// Keep in sync with internal/fuzz.workerExitCode.
const fuzzWorkerExitCode = 70

// InternalFuzzTarget is an internal type but exported because it is cross-package;
// it is part of the implementation of the "go test" command.
type InternalFuzzTarget struct {
	Name string
	Fn   func(f *F)
}

// F is a type passed to fuzz targets.
//
// A fuzz target may add seed corpus entries using F.Add or by storing files in
// the testdata/fuzz/<FuzzTargetName> directory. The fuzz target must then
// call F.Fuzz once to provide a fuzz function. See the testing package
// documentation for an example, and see the F.Fuzz and F.Add method
// documentation for details.
type F struct {
	common
	fuzzContext *fuzzContext
	testContext *testContext

	// inFuzzFn is true when the fuzz function is running. Most F methods cannot
	// be called when inFuzzFn is true.
	inFuzzFn bool

	// corpus is a set of seed corpus entries, added with F.Add and loaded
	// from testdata.
	corpus []corpusEntry

	result     FuzzResult
	fuzzCalled bool
}

var _ TB = (*F)(nil)

// corpusEntry is an alias to the same type as internal/fuzz.CorpusEntry.
// We use a type alias because we don't want to export this type, and we can't
// import internal/fuzz from testing.
type corpusEntry = struct {
	Parent     string
	Name       string
	Data       []byte
	Values     []interface{}
	Generation int
	IsSeed     bool
}

// Cleanup registers a function to be called after the fuzz function has been
// called on all seed corpus entries, and after fuzzing completes (if enabled).
// Cleanup functions will be called in last added, first called order.
func (f *F) Cleanup(fn func()) {
	if f.inFuzzFn {
		panic("testing: f.Cleanup was called inside the f.Fuzz function, use t.Cleanup instead")
	}
	f.common.Helper()
	f.common.Cleanup(fn)
}

// Error is equivalent to Log followed by Fail.
func (f *F) Error(args ...interface{}) {
	if f.inFuzzFn {
		panic("testing: f.Error was called inside the f.Fuzz function, use t.Error instead")
	}
	f.common.Helper()
	f.common.Error(args...)
}

// Errorf is equivalent to Logf followed by Fail.
func (f *F) Errorf(format string, args ...interface{}) {
	if f.inFuzzFn {
		panic("testing: f.Errorf was called inside the f.Fuzz function, use t.Errorf instead")
	}
	f.common.Helper()
	f.common.Errorf(format, args...)
}

// Fail marks the function as having failed but continues execution.
func (f *F) Fail() {
	if f.inFuzzFn {
		panic("testing: f.Fail was called inside the f.Fuzz function, use t.Fail instead")
	}
	f.common.Helper()
	f.common.Fail()
}

// FailNow marks the function as having failed and stops its execution
// by calling runtime.Goexit (which then runs all deferred calls in the
// current goroutine).
// Execution will continue at the next test, benchmark, or fuzz function.
// FailNow must be called from the goroutine running the
// fuzz target, not from other goroutines
// created during the test. Calling FailNow does not stop
// those other goroutines.
func (f *F) FailNow() {
	if f.inFuzzFn {
		panic("testing: f.FailNow was called inside the f.Fuzz function, use t.FailNow instead")
	}
	f.common.Helper()
	f.common.FailNow()
}

// Fatal is equivalent to Log followed by FailNow.
func (f *F) Fatal(args ...interface{}) {
	if f.inFuzzFn {
		panic("testing: f.Fatal was called inside the f.Fuzz function, use t.Fatal instead")
	}
	f.common.Helper()
	f.common.Fatal(args...)
}

// Fatalf is equivalent to Logf followed by FailNow.
func (f *F) Fatalf(format string, args ...interface{}) {
	if f.inFuzzFn {
		panic("testing: f.Fatalf was called inside the f.Fuzz function, use t.Fatalf instead")
	}
	f.common.Helper()
	f.common.Fatalf(format, args...)
}

// Helper marks the calling function as a test helper function.
// When printing file and line information, that function will be skipped.
// Helper may be called simultaneously from multiple goroutines.
func (f *F) Helper() {
	if f.inFuzzFn {
		panic("testing: f.Helper was called inside the f.Fuzz function, use t.Helper instead")
	}

	// common.Helper is inlined here.
	// If we called it, it would mark F.Helper as the helper
	// instead of the caller.
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.helperPCs == nil {
		f.helperPCs = make(map[uintptr]struct{})
	}
	// repeating code from callerName here to save walking a stack frame
	var pc [1]uintptr
	n := runtime.Callers(2, pc[:]) // skip runtime.Callers + Helper
	if n == 0 {
		panic("testing: zero callers found")
	}
	if _, found := f.helperPCs[pc[0]]; !found {
		f.helperPCs[pc[0]] = struct{}{}
		f.helperNames = nil // map will be recreated next time it is needed
	}
}

// Setenv calls os.Setenv(key, value) and uses Cleanup to restore the
// environment variable to its original value after the test.
//
// When fuzzing is enabled, the fuzzing engine spawns worker processes running
// the test binary. Each worker process inherits the environment of the parent
// process, including environment variables set with F.Setenv.
func (f *F) Setenv(key, value string) {
	if f.inFuzzFn {
		panic("testing: f.Setenv was called inside the f.Fuzz function, use t.Setenv instead")
	}
	f.common.Helper()
	f.common.Setenv(key, value)
}

// Skip is equivalent to Log followed by SkipNow.
func (f *F) Skip(args ...interface{}) {
	if f.inFuzzFn {
		panic("testing: f.Skip was called inside the f.Fuzz function, use t.Skip instead")
	}
	f.common.Helper()
	f.common.Skip(args...)
}

// SkipNow marks the test as having been skipped and stops its execution
// by calling runtime.Goexit.
// If a test fails (see Error, Errorf, Fail) and is then skipped,
// it is still considered to have failed.
// Execution will continue at the next test or benchmark. See also FailNow.
// SkipNow must be called from the goroutine running the test, not from
// other goroutines created during the test. Calling SkipNow does not stop
// those other goroutines.
func (f *F) SkipNow() {
	if f.inFuzzFn {
		panic("testing: f.SkipNow was called inside the f.Fuzz function, use t.SkipNow instead")
	}
	f.common.Helper()
	f.common.SkipNow()
}

// Skipf is equivalent to Logf followed by SkipNow.
func (f *F) Skipf(format string, args ...interface{}) {
	if f.inFuzzFn {
		panic("testing: f.Skipf was called inside the f.Fuzz function, use t.Skipf instead")
	}
	f.common.Helper()
	f.common.Skipf(format, args...)
}

// TempDir returns a temporary directory for the test to use.
// The directory is automatically removed by Cleanup when the test and
// all its subtests complete.
// Each subsequent call to t.TempDir returns a unique directory;
// if the directory creation fails, TempDir terminates the test by calling Fatal.
func (f *F) TempDir() string {
	if f.inFuzzFn {
		panic("testing: f.TempDir was called inside the f.Fuzz function, use t.TempDir instead")
	}
	f.common.Helper()
	return f.common.TempDir()
}

// Add will add the arguments to the seed corpus for the fuzz target. This will
// be a no-op if called after or within the Fuzz function. The args must match
// those in the Fuzz function.
func (f *F) Add(args ...interface{}) {
	var values []interface{}
	for i := range args {
		if t := reflect.TypeOf(args[i]); !supportedTypes[t] {
			panic(fmt.Sprintf("testing: unsupported type to Add %v", t))
		}
		values = append(values, args[i])
	}
	f.corpus = append(f.corpus, corpusEntry{Values: values, IsSeed: true, Name: fmt.Sprintf("seed#%d", len(f.corpus))})
}

// supportedTypes represents all of the supported types which can be fuzzed.
var supportedTypes = map[reflect.Type]bool{
	reflect.TypeOf(([]byte)("")):  true,
	reflect.TypeOf((string)("")):  true,
	reflect.TypeOf((bool)(false)): true,
	reflect.TypeOf((byte)(0)):     true,
	reflect.TypeOf((rune)(0)):     true,
	reflect.TypeOf((float32)(0)):  true,
	reflect.TypeOf((float64)(0)):  true,
	reflect.TypeOf((int)(0)):      true,
	reflect.TypeOf((int8)(0)):     true,
	reflect.TypeOf((int16)(0)):    true,
	reflect.TypeOf((int32)(0)):    true,
	reflect.TypeOf((int64)(0)):    true,
	reflect.TypeOf((uint)(0)):     true,
	reflect.TypeOf((uint8)(0)):    true,
	reflect.TypeOf((uint16)(0)):   true,
	reflect.TypeOf((uint32)(0)):   true,
	reflect.TypeOf((uint64)(0)):   true,
}

// Fuzz runs the fuzz function, ff, for fuzz testing. If ff fails for a set of
// arguments, those arguments will be added to the seed corpus.
//
// ff must be a function with no return value whose first argument is *T and
// whose remaining arguments are the types to be fuzzed.
// For example:
//
// f.Fuzz(func(t *testing.T, b []byte, i int) { ... })
//
// This function should be fast, deterministic, and stateless.
//
// No mutatable input arguments, or pointers to them, should be retained between
// executions of the fuzz function, as the memory backing them may be mutated
// during a subsequent invocation.
//
// This is a terminal function which will terminate the currently running fuzz
// target by calling runtime.Goexit.
// To run any code after fuzzing stops, use (*F).Cleanup.
func (f *F) Fuzz(ff interface{}) {
	if f.fuzzCalled {
		panic("testing: F.Fuzz called more than once")
	}
	f.fuzzCalled = true
	if f.failed {
		return
	}
	f.Helper()

	// ff should be in the form func(*testing.T, ...interface{})
	fn := reflect.ValueOf(ff)
	fnType := fn.Type()
	if fnType.Kind() != reflect.Func {
		panic("testing: F.Fuzz must receive a function")
	}
	if fnType.NumIn() < 2 || fnType.In(0) != reflect.TypeOf((*T)(nil)) {
		panic("testing: F.Fuzz function must receive at least two arguments, where the first argument is a *T")
	}

	// Save the types of the function to compare against the corpus.
	var types []reflect.Type
	for i := 1; i < fnType.NumIn(); i++ {
		t := fnType.In(i)
		if !supportedTypes[t] {
			panic(fmt.Sprintf("testing: unsupported type for fuzzing %v", t))
		}
		types = append(types, t)
	}

	// Load the testdata seed corpus. Check types of entries in the testdata
	// corpus and entries declared with F.Add.
	//
	// Don't load the seed corpus if this is a worker process; we won't use it.
	if f.fuzzContext.mode != fuzzWorker {
		for _, c := range f.corpus {
			if err := f.fuzzContext.deps.CheckCorpus(c.Values, types); err != nil {
				// TODO(#48302): Report the source location of the F.Add call.
				f.Fatal(err)
			}
		}

		// Load seed corpus
		c, err := f.fuzzContext.deps.ReadCorpus(filepath.Join(corpusDir, f.name), types)
		if err != nil {
			f.Fatal(err)
		}
		for i := range c {
			c[i].IsSeed = true // these are all seed corpus values
			if f.fuzzContext.mode == fuzzCoordinator {
				// If this is the coordinator process, zero the values, since we don't need
				// to hold onto them.
				c[i].Values = nil
			}
		}

		f.corpus = append(f.corpus, c...)
	}

	// run calls fn on a given input, as a subtest with its own T.
	// run is analogous to T.Run. The test filtering and cleanup works similarly.
	// fn is called in its own goroutine.
	run := func(e corpusEntry) error {
		if e.Values == nil {
			// Every code path should have already unmarshaled Data into Values.
			// It's our fault if it didn't.
			panic(fmt.Sprintf("corpus file %q was not unmarshaled", e.Name))
		}
		if shouldFailFast() {
			return nil
		}
		testName := f.common.name
		if e.Name != "" {
			testName = fmt.Sprintf("%s/%s", testName, e.Name)
		}

		// Record the stack trace at the point of this call so that if the subtest
		// function - which runs in a separate stack - is marked as a helper, we can
		// continue walking the stack into the parent test.
		var pc [maxStackLen]uintptr
		n := runtime.Callers(2, pc[:])
		t := &T{
			common: common{
				barrier: make(chan bool),
				signal:  make(chan bool),
				name:    testName,
				parent:  &f.common,
				level:   f.level + 1,
				creator: pc[:n],
				chatty:  f.chatty,
				fuzzing: true,
			},
			context: f.testContext,
		}
		t.w = indenter{&t.common}
		if t.chatty != nil {
			// TODO(#48132): adjust this to work with test2json.
			t.chatty.Updatef(t.name, "=== RUN   %s\n", t.name)
		}
		f.inFuzzFn = true
		go tRunner(t, func(t *T) {
			args := []reflect.Value{reflect.ValueOf(t)}
			for _, v := range e.Values {
				args = append(args, reflect.ValueOf(v))
			}
			// Before reseting the current coverage, defer the snapshot so that we
			// make sure it is called right before the tRunner function exits,
			// regardless of whether it was executed cleanly, panicked, or if the
			// fuzzFn called t.Fatal.
			defer f.fuzzContext.deps.SnapshotCoverage()
			f.fuzzContext.deps.ResetCoverage()
			fn.Call(args)
		})
		<-t.signal
		f.inFuzzFn = false
		if t.Failed() {
			return errors.New(string(f.output))
		}
		return nil
	}

	switch f.fuzzContext.mode {
	case fuzzCoordinator:
		// Fuzzing is enabled, and this is the test process started by 'go test'.
		// Act as the coordinator process, and coordinate workers to perform the
		// actual fuzzing.
		corpusTargetDir := filepath.Join(corpusDir, f.name)
		cacheTargetDir := filepath.Join(*fuzzCacheDir, f.name)
		err := f.fuzzContext.deps.CoordinateFuzzing(
			fuzzDuration.d,
			int64(fuzzDuration.n),
			minimizeDuration.d,
			int64(minimizeDuration.n),
			*parallel,
			f.corpus,
			types,
			corpusTargetDir,
			cacheTargetDir)
		if err != nil {
			f.result = FuzzResult{Error: err}
			f.Fail()
			fmt.Fprintf(f.w, "%v\n", err)
			if crashErr, ok := err.(fuzzCrashError); ok {
				crashName := crashErr.CrashName()
				fmt.Fprintf(f.w, "Crash written to %s\n", filepath.Join(corpusDir, f.name, crashName))
				fmt.Fprintf(f.w, "To re-run:\ngo test %s -run=%s/%s\n", f.fuzzContext.deps.ImportPath(), f.name, crashName)
			}
		}
		// TODO(jayconrod,katiehockman): Aggregate statistics across workers
		// and add to FuzzResult (ie. time taken, num iterations)

	case fuzzWorker:
		// Fuzzing is enabled, and this is a worker process. Follow instructions
		// from the coordinator.
		if err := f.fuzzContext.deps.RunFuzzWorker(run); err != nil {
			// Internal errors are marked with f.Fail; user code may call this too, before F.Fuzz.
			// The worker will exit with fuzzWorkerExitCode, indicating this is a failure
			// (and 'go test' should exit non-zero) but a crasher should not be recorded.
			f.Errorf("communicating with fuzzing coordinator: %v", err)
		}

	default:
		// Fuzzing is not enabled, or will be done later. Only run the seed
		// corpus now.
		for _, e := range f.corpus {
			run(e)
		}
	}

	// Record that the fuzz function (or coordinateFuzzing or runFuzzWorker)
	// returned normally. This is used to distinguish runtime.Goexit below
	// from panic(nil).
	f.finished = true

	// Terminate the goroutine. F.Fuzz should not return.
	// We cannot call runtime.Goexit from a deferred function: if there is a
	// panic, that would replace the panic value with nil.
	runtime.Goexit()
}

func (f *F) report() {
	if *isFuzzWorker || f.parent == nil {
		return
	}
	dstr := fmtDuration(f.duration)
	format := "--- %s: %s (%s)\n"
	if f.Failed() {
		f.flushToParent(f.name, format, "FAIL", f.name, dstr)
	} else if f.chatty != nil {
		if f.Skipped() {
			f.flushToParent(f.name, format, "SKIP", f.name, dstr)
		} else {
			f.flushToParent(f.name, format, "PASS", f.name, dstr)
		}
	}
}

// FuzzResult contains the results of a fuzz run.
type FuzzResult struct {
	N     int           // The number of iterations.
	T     time.Duration // The total time taken.
	Error error         // Error is the error from the crash
}

func (r FuzzResult) String() string {
	s := ""
	if r.Error == nil {
		return s
	}
	s = fmt.Sprintf("%s", r.Error.Error())
	return s
}

// fuzzCrashError is satisfied by a crash detected within the fuzz function.
// These errors are written to the seed corpus and can be re-run with 'go test'.
// Errors within the fuzzing framework (like I/O errors between coordinator
// and worker processes) don't satisfy this interface.
type fuzzCrashError interface {
	error
	Unwrap() error

	// CrashName returns the name of the subtest that corresponds to the saved
	// crash input file in the seed corpus. The test can be re-run with
	// go test $pkg -run=$target/$name where $pkg is the package's import path,
	// $target is the fuzz target name, and $name is the string returned here.
	CrashName() string
}

// fuzzContext holds fields common to all fuzz targets.
type fuzzContext struct {
	deps testDeps
	mode fuzzMode
}

type fuzzMode uint8

const (
	seedCorpusOnly fuzzMode = iota
	fuzzCoordinator
	fuzzWorker
)

// runFuzzTargets runs the fuzz targets matching the pattern for -run. This will
// only run the f.Fuzz function for each seed corpus without using the fuzzing
// engine to generate or mutate inputs.
func runFuzzTargets(deps testDeps, fuzzTargets []InternalFuzzTarget, deadline time.Time) (ran, ok bool) {
	ok = true
	if len(fuzzTargets) == 0 || *isFuzzWorker {
		return ran, ok
	}
	m := newMatcher(deps.MatchString, *match, "-test.run")
	tctx := newTestContext(*parallel, m)
	tctx.deadline = deadline
	var mFuzz *matcher
	if *matchFuzz != "" {
		mFuzz = newMatcher(deps.MatchString, *matchFuzz, "-test.fuzz")
	}
	fctx := &fuzzContext{deps: deps, mode: seedCorpusOnly}
	root := common{w: os.Stdout} // gather output in one place
	if Verbose() {
		root.chatty = newChattyPrinter(root.w)
	}
	for _, ft := range fuzzTargets {
		if shouldFailFast() {
			break
		}
		testName, matched, _ := tctx.match.fullName(nil, ft.Name)
		if !matched {
			continue
		}
		if mFuzz != nil {
			if _, fuzzMatched, _ := mFuzz.fullName(nil, ft.Name); fuzzMatched {
				// If this target will be fuzzed, then don't run the seed corpus
				// right now. That will happen later.
				continue
			}
		}
		f := &F{
			common: common{
				signal:  make(chan bool),
				barrier: make(chan bool),
				name:    testName,
				parent:  &root,
				level:   root.level + 1,
				chatty:  root.chatty,
			},
			testContext: tctx,
			fuzzContext: fctx,
		}
		f.w = indenter{&f.common}
		if f.chatty != nil {
			// TODO(#48132): adjust this to work with test2json.
			f.chatty.Updatef(f.name, "=== RUN   %s\n", f.name)
		}

		go fRunner(f, ft.Fn)
		<-f.signal
	}
	return root.ran, !root.Failed()
}

// runFuzzing runs the fuzz target matching the pattern for -fuzz. Only one such
// fuzz target must match. This will run the fuzzing engine to generate and
// mutate new inputs against the f.Fuzz function.
//
// If fuzzing is disabled (-test.fuzz is not set), runFuzzing
// returns immediately.
func runFuzzing(deps testDeps, fuzzTargets []InternalFuzzTarget) (ok bool) {
	// TODO(katiehockman,jayconrod): Should we do something special to make sure
	// we don't print f.Log statements again with runFuzzing, since we already
	// would have printed them when we ran runFuzzTargets (ie. seed corpus run)?
	if len(fuzzTargets) == 0 || *matchFuzz == "" {
		return true
	}
	m := newMatcher(deps.MatchString, *matchFuzz, "-test.fuzz")
	tctx := newTestContext(1, m)
	fctx := &fuzzContext{
		deps: deps,
	}
	root := common{w: os.Stdout}
	if *isFuzzWorker {
		root.w = io.Discard
		fctx.mode = fuzzWorker
	} else {
		fctx.mode = fuzzCoordinator
	}
	if Verbose() && !*isFuzzWorker {
		root.chatty = newChattyPrinter(root.w)
	}
	var target *InternalFuzzTarget
	var targetName string
	var matched []string
	for i := range fuzzTargets {
		name, ok, _ := tctx.match.fullName(nil, fuzzTargets[i].Name)
		if !ok {
			continue
		}
		matched = append(matched, name)
		target = &fuzzTargets[i]
		targetName = name
	}
	if len(matched) == 0 {
		fmt.Fprintln(os.Stderr, "testing: warning: no targets to fuzz")
		return true
	}
	if len(matched) > 1 {
		fmt.Fprintf(os.Stderr, "testing: will not fuzz, -fuzz matches more than one target: %v\n", matched)
		return false
	}

	f := &F{
		common: common{
			signal:  make(chan bool),
			barrier: nil, // T.Parallel has no effect when fuzzing.
			name:    targetName,
			parent:  &root,
			level:   root.level + 1,
			chatty:  root.chatty,
		},
		fuzzContext: fctx,
		testContext: tctx,
	}
	f.w = indenter{&f.common}
	if f.chatty != nil {
		// TODO(#48132): adjust this to work with test2json.
		f.chatty.Updatef(f.name, "=== FUZZ  %s\n", f.name)
	}
	go fRunner(f, target.Fn)
	<-f.signal
	return !f.failed
}

// fRunner wraps a call to a fuzz target and ensures that cleanup functions are
// called and status flags are set. fRunner should be called in its own
// goroutine. To wait for its completion, receive from f.signal.
//
// fRunner is analogous to tRunner, which wraps subtests started with T.Run.
// Tests and fuzz targets work a little differently, so for now, these functions
// aren't consolidated. In particular, because there are no F.Run and F.Parallel
// methods, i.e., no fuzz sub-targets or parallel fuzz targets, a few
// simplifications are made. We also require that F.Fuzz, F.Skip, or F.Fail is
// called.
func fRunner(f *F, fn func(*F)) {
	// When this goroutine is done, either because runtime.Goexit was called,
	// a panic started, or fn returned normally, record the duration and send
	// t.signal, indicating the fuzz target is done.
	defer func() {
		// Detect whether the fuzz target panicked or called runtime.Goexit without
		// calling F.Fuzz, F.Fail, or F.Skip. If it did, panic (possibly replacing a
		// nil panic value). Nothing should recover after fRunner unwinds, so this
		// should crash the process and print stack. Unfortunately, recovering here
		// adds stack frames, but the location of the original panic should still be
		// clear.
		if f.Failed() {
			atomic.AddUint32(&numFailed, 1)
		}
		err := recover()
		f.mu.RLock()
		ok := f.skipped || f.failed || (f.fuzzCalled && f.finished)
		f.mu.RUnlock()
		if err == nil && !ok {
			err = errNilPanicOrGoexit
		}

		// Use a deferred call to ensure that we report that the test is
		// complete even if a cleanup function calls t.FailNow. See issue 41355.
		didPanic := false
		defer func() {
			if didPanic {
				return
			}
			if err != nil {
				panic(err)
			}
			// Only report that the test is complete if it doesn't panic,
			// as otherwise the test binary can exit before the panic is
			// reported to the user. See issue 41479.
			f.signal <- true
		}()

		// If we recovered a panic or inappropriate runtime.Goexit, fail the test,
		// flush the output log up to the root, then panic.
		doPanic := func(err interface{}) {
			f.Fail()
			if r := f.runCleanup(recoverAndReturnPanic); r != nil {
				f.Logf("cleanup panicked with %v", r)
			}
			for root := &f.common; root.parent != nil; root = root.parent {
				root.mu.Lock()
				root.duration += time.Since(root.start)
				d := root.duration
				root.mu.Unlock()
				root.flushToParent(root.name, "--- FAIL: %s (%s)\n", root.name, fmtDuration(d))
			}
			didPanic = true
			panic(err)
		}
		if err != nil {
			doPanic(err)
		}

		// No panic or inappropriate Goexit.
		f.duration += time.Since(f.start)

		if len(f.sub) > 0 {
			// Unblock inputs that called T.Parallel while running the seed corpus.
			// T.Parallel has no effect while fuzzing, so this only affects fuzz
			// targets run as normal tests.
			close(f.barrier)
			// Wait for the subtests to complete.
			for _, sub := range f.sub {
				<-sub.signal
			}
			cleanupStart := time.Now()
			err := f.runCleanup(recoverAndReturnPanic)
			f.duration += time.Since(cleanupStart)
			if err != nil {
				doPanic(err)
			}
		}

		// Report after all subtests have finished.
		f.report()
		f.done = true
		f.setRan()
	}()
	defer func() {
		if len(f.sub) == 0 {
			f.runCleanup(normalPanic)
		}
	}()

	f.start = time.Now()
	fn(f)

	// Code beyond this point is only executed if fn returned normally.
	// That means fn did not call F.Fuzz or F.Skip. It should have called F.Fail.
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.failed {
		panic(f.name + " returned without calling F.Fuzz, F.Fail, or F.Skip")
	}
}
