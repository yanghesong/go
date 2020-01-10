// Copyright 2015 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main_test

import (
	"bytes"
	"context"
	"debug/elf"
	"debug/macho"
	"flag"
	"fmt"
	"go/format"
	"internal/race"
	"internal/testenv"
	"io"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"cmd/go/internal/cache"
	"cmd/go/internal/cfg"
	"cmd/go/internal/robustio"
	"cmd/internal/sys"
)

var (
	canRun  = true  // whether we can run go or ./testgo
	canRace = false // whether we can run the race detector
	canCgo  = false // whether we can use cgo
	canMSan = false // whether we can run the memory sanitizer

	exeSuffix string // ".exe" on Windows

	skipExternal = false // skip external tests
)

func tooSlow(t *testing.T) {
	if testing.Short() {
		// In -short mode; skip test, except run it on the {darwin,linux,windows}/amd64 builders.
		if testenv.Builder() != "" && runtime.GOARCH == "amd64" && (runtime.GOOS == "linux" || runtime.GOOS == "darwin" || runtime.GOOS == "windows") {
			return
		}
		t.Skip("skipping test in -short mode")
	}
}

func init() {
	switch runtime.GOOS {
	case "android", "js":
		canRun = false
	case "darwin":
		switch runtime.GOARCH {
		case "arm", "arm64":
			canRun = false
		}
	case "linux":
		switch runtime.GOARCH {
		case "arm":
			// many linux/arm machines are too slow to run
			// the full set of external tests.
			skipExternal = true
		case "mips", "mipsle", "mips64", "mips64le":
			// Also slow.
			skipExternal = true
			if testenv.Builder() != "" {
				// On the builders, skip the cmd/go
				// tests. They're too slow and already
				// covered by other ports. There's
				// nothing os/arch specific in the
				// tests.
				canRun = false
			}
		}
	case "freebsd":
		switch runtime.GOARCH {
		case "arm":
			// many freebsd/arm machines are too slow to run
			// the full set of external tests.
			skipExternal = true
			canRun = false
		}
	case "plan9":
		switch runtime.GOARCH {
		case "arm":
			// many plan9/arm machines are too slow to run
			// the full set of external tests.
			skipExternal = true
		}
	case "windows":
		exeSuffix = ".exe"
	}
}

// testGOROOT is the GOROOT to use when running testgo, a cmd/go binary
// build from this process's current GOROOT, but run from a different
// (temp) directory.
var testGOROOT string

var testCC string
var testGOCACHE string

var testGo string
var testTmpDir string
var testBin string

// testCtx is canceled when the test binary is about to time out.
//
// If https://golang.org/issue/28135 is accepted, uses of this variable in test
// functions should be replaced by t.Context().
var testCtx = context.Background()

// The TestMain function creates a go command for testing purposes and
// deletes it after the tests have been run.
func TestMain(m *testing.M) {
	// $GO_GCFLAGS a compiler debug flag known to cmd/dist, make.bash, etc.
	// It is not a standard go command flag; use os.Getenv, not cfg.Getenv.
	if os.Getenv("GO_GCFLAGS") != "" {
		fmt.Fprintf(os.Stderr, "testing: warning: no tests to run\n") // magic string for cmd/go
		fmt.Printf("cmd/go test is not compatible with $GO_GCFLAGS being set\n")
		fmt.Printf("SKIP\n")
		return
	}
	os.Unsetenv("GOROOT_FINAL")

	flag.Parse()

	timeoutFlag := flag.Lookup("test.timeout")
	if timeoutFlag != nil {
		// TODO(golang.org/issue/28147): The go command does not pass the
		// test.timeout flag unless either -timeout or -test.timeout is explicitly
		// set on the command line.
		if d := timeoutFlag.Value.(flag.Getter).Get().(time.Duration); d != 0 {
			aBitShorter := d * 95 / 100
			var cancel context.CancelFunc
			testCtx, cancel = context.WithTimeout(testCtx, aBitShorter)
			defer cancel()
		}
	}

	if *proxyAddr != "" {
		StartProxy()
		select {}
	}

	// Run with a temporary TMPDIR to check that the tests don't
	// leave anything behind.
	topTmpdir, err := ioutil.TempDir("", "cmd-go-test-")
	if err != nil {
		log.Fatal(err)
	}
	if !*testWork {
		defer removeAll(topTmpdir)
	}
	os.Setenv(tempEnvName(), topTmpdir)

	dir, err := ioutil.TempDir(topTmpdir, "tmpdir")
	if err != nil {
		log.Fatal(err)
	}
	testTmpDir = dir
	if !*testWork {
		defer removeAll(testTmpDir)
	}

	testGOCACHE = cache.DefaultDir()
	if canRun {
		testBin = filepath.Join(testTmpDir, "testbin")
		if err := os.Mkdir(testBin, 0777); err != nil {
			log.Fatal(err)
		}
		testGo = filepath.Join(testBin, "go"+exeSuffix)
		args := []string{"build", "-tags", "testgo", "-o", testGo}
		if race.Enabled {
			args = append(args, "-race")
		}
		gotool, err := testenv.GoTool()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}

		goEnv := func(name string) string {
			out, err := exec.Command(gotool, "env", name).CombinedOutput()
			if err != nil {
				fmt.Fprintf(os.Stderr, "go env %s: %v\n%s", name, err, out)
				os.Exit(2)
			}
			return strings.TrimSpace(string(out))
		}
		testGOROOT = goEnv("GOROOT")

		// The whole GOROOT/pkg tree was installed using the GOHOSTOS/GOHOSTARCH
		// toolchain (installed in GOROOT/pkg/tool/GOHOSTOS_GOHOSTARCH).
		// The testgo.exe we are about to create will be built for GOOS/GOARCH,
		// which means it will use the GOOS/GOARCH toolchain
		// (installed in GOROOT/pkg/tool/GOOS_GOARCH).
		// If these are not the same toolchain, then the entire standard library
		// will look out of date (the compilers in those two different tool directories
		// are built for different architectures and have different build IDs),
		// which will cause many tests to do unnecessary rebuilds and some
		// tests to attempt to overwrite the installed standard library.
		// Bail out entirely in this case.
		hostGOOS := goEnv("GOHOSTOS")
		hostGOARCH := goEnv("GOHOSTARCH")
		if hostGOOS != runtime.GOOS || hostGOARCH != runtime.GOARCH {
			fmt.Fprintf(os.Stderr, "testing: warning: no tests to run\n") // magic string for cmd/go
			fmt.Printf("cmd/go test is not compatible with GOOS/GOARCH != GOHOSTOS/GOHOSTARCH (%s/%s != %s/%s)\n", runtime.GOOS, runtime.GOARCH, hostGOOS, hostGOARCH)
			fmt.Printf("SKIP\n")
			return
		}

		buildCmd := exec.Command(gotool, args...)
		buildCmd.Env = append(os.Environ(), "GOFLAGS=-mod=vendor")
		out, err := buildCmd.CombinedOutput()
		if err != nil {
			fmt.Fprintf(os.Stderr, "building testgo failed: %v\n%s", err, out)
			os.Exit(2)
		}

		out, err = exec.Command(gotool, "env", "CC").CombinedOutput()
		if err != nil {
			fmt.Fprintf(os.Stderr, "could not find testing CC: %v\n%s", err, out)
			os.Exit(2)
		}
		testCC = strings.TrimSpace(string(out))

		if out, err := exec.Command(testGo, "env", "CGO_ENABLED").Output(); err != nil {
			fmt.Fprintf(os.Stderr, "running testgo failed: %v\n", err)
			canRun = false
		} else {
			canCgo, err = strconv.ParseBool(strings.TrimSpace(string(out)))
			if err != nil {
				fmt.Fprintf(os.Stderr, "can't parse go env CGO_ENABLED output: %v\n", strings.TrimSpace(string(out)))
			}
		}

		out, err = exec.Command(gotool, "env", "GOCACHE").CombinedOutput()
		if err != nil {
			fmt.Fprintf(os.Stderr, "could not find testing GOCACHE: %v\n%s", err, out)
			os.Exit(2)
		}
		testGOCACHE = strings.TrimSpace(string(out))

		canMSan = canCgo && sys.MSanSupported(runtime.GOOS, runtime.GOARCH)
		canRace = canCgo && sys.RaceDetectorSupported(runtime.GOOS, runtime.GOARCH)
		// The race detector doesn't work on Alpine Linux:
		// golang.org/issue/14481
		// gccgo does not support the race detector.
		if isAlpineLinux() || runtime.Compiler == "gccgo" {
			canRace = false
		}
	}
	// Don't let these environment variables confuse the test.
	os.Setenv("GOENV", "off")
	os.Unsetenv("GOBIN")
	os.Unsetenv("GOPATH")
	os.Unsetenv("GIT_ALLOW_PROTOCOL")
	os.Setenv("HOME", "/test-go-home-does-not-exist")
	// On some systems the default C compiler is ccache.
	// Setting HOME to a non-existent directory will break
	// those systems. Disable ccache and use real compiler. Issue 17668.
	os.Setenv("CCACHE_DISABLE", "1")
	if cfg.Getenv("GOCACHE") == "" {
		os.Setenv("GOCACHE", testGOCACHE) // because $HOME is gone
	}

	r := m.Run()
	if !*testWork {
		removeAll(testTmpDir) // os.Exit won't run defer
	}

	if !*testWork {
		// There shouldn't be anything left in topTmpdir.
		dirf, err := os.Open(topTmpdir)
		if err != nil {
			log.Fatal(err)
		}
		names, err := dirf.Readdirnames(0)
		if err != nil {
			log.Fatal(err)
		}
		if len(names) > 0 {
			log.Fatalf("unexpected files left in tmpdir: %v", names)
		}

		removeAll(topTmpdir)
	}

	os.Exit(r)
}

func isAlpineLinux() bool {
	if runtime.GOOS != "linux" {
		return false
	}
	fi, err := os.Lstat("/etc/alpine-release")
	return err == nil && fi.Mode().IsRegular()
}

// The length of an mtime tick on this system. This is an estimate of
// how long we need to sleep to ensure that the mtime of two files is
// different.
// We used to try to be clever but that didn't always work (see golang.org/issue/12205).
var mtimeTick time.Duration = 1 * time.Second

// Manage a single run of the testgo binary.
type testgoData struct {
	t              *testing.T
	temps          []string
	wd             string
	env            []string
	tempdir        string
	ran            bool
	inParallel     bool
	stdout, stderr bytes.Buffer
	execDir        string // dir for tg.run
}

// skipIfGccgo skips the test if using gccgo.
func skipIfGccgo(t *testing.T, msg string) {
	if runtime.Compiler == "gccgo" {
		t.Skipf("skipping test not supported on gccgo: %s", msg)
	}
}

// testgo sets up for a test that runs testgo.
func testgo(t *testing.T) *testgoData {
	t.Helper()
	testenv.MustHaveGoBuild(t)

	if skipExternal {
		t.Skipf("skipping external tests on %s/%s", runtime.GOOS, runtime.GOARCH)
	}

	return &testgoData{t: t}
}

// must gives a fatal error if err is not nil.
func (tg *testgoData) must(err error) {
	tg.t.Helper()
	if err != nil {
		tg.t.Fatal(err)
	}
}

// check gives a test non-fatal error if err is not nil.
func (tg *testgoData) check(err error) {
	tg.t.Helper()
	if err != nil {
		tg.t.Error(err)
	}
}

// parallel runs the test in parallel by calling t.Parallel.
func (tg *testgoData) parallel() {
	tg.t.Helper()
	if tg.ran {
		tg.t.Fatal("internal testsuite error: call to parallel after run")
	}
	if tg.wd != "" {
		tg.t.Fatal("internal testsuite error: call to parallel after cd")
	}
	for _, e := range tg.env {
		if strings.HasPrefix(e, "GOROOT=") || strings.HasPrefix(e, "GOPATH=") || strings.HasPrefix(e, "GOBIN=") {
			val := e[strings.Index(e, "=")+1:]
			if strings.HasPrefix(val, "testdata") || strings.HasPrefix(val, "./testdata") {
				tg.t.Fatalf("internal testsuite error: call to parallel with testdata in environment (%s)", e)
			}
		}
	}
	tg.inParallel = true
	tg.t.Parallel()
}

// pwd returns the current directory.
func (tg *testgoData) pwd() string {
	tg.t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		tg.t.Fatalf("could not get working directory: %v", err)
	}
	return wd
}

// cd changes the current directory to the named directory. Note that
// using this means that the test must not be run in parallel with any
// other tests.
func (tg *testgoData) cd(dir string) {
	tg.t.Helper()
	if tg.inParallel {
		tg.t.Fatal("internal testsuite error: changing directory when running in parallel")
	}
	if tg.wd == "" {
		tg.wd = tg.pwd()
	}
	abs, err := filepath.Abs(dir)
	tg.must(os.Chdir(dir))
	if err == nil {
		tg.setenv("PWD", abs)
	}
}

// sleep sleeps for one tick, where a tick is a conservative estimate
// of how long it takes for a file modification to get a different
// mtime.
func (tg *testgoData) sleep() {
	time.Sleep(mtimeTick)
}

// setenv sets an environment variable to use when running the test go
// command.
func (tg *testgoData) setenv(name, val string) {
	tg.t.Helper()
	if tg.inParallel && (name == "GOROOT" || name == "GOPATH" || name == "GOBIN") && (strings.HasPrefix(val, "testdata") || strings.HasPrefix(val, "./testdata")) {
		tg.t.Fatalf("internal testsuite error: call to setenv with testdata (%s=%s) after parallel", name, val)
	}
	tg.unsetenv(name)
	tg.env = append(tg.env, name+"="+val)
}

// unsetenv removes an environment variable.
func (tg *testgoData) unsetenv(name string) {
	if tg.env == nil {
		tg.env = append([]string(nil), os.Environ()...)
		tg.env = append(tg.env, "GO111MODULE=off")
	}
	for i, v := range tg.env {
		if strings.HasPrefix(v, name+"=") {
			tg.env = append(tg.env[:i], tg.env[i+1:]...)
			break
		}
	}
}

func (tg *testgoData) goTool() string {
	return testGo
}

// doRun runs the test go command, recording stdout and stderr and
// returning exit status.
func (tg *testgoData) doRun(args []string) error {
	tg.t.Helper()
	if !canRun {
		panic("testgoData.doRun called but canRun false")
	}
	if tg.inParallel {
		for _, arg := range args {
			if strings.HasPrefix(arg, "testdata") || strings.HasPrefix(arg, "./testdata") {
				tg.t.Fatal("internal testsuite error: parallel run using testdata")
			}
		}
	}

	hasGoroot := false
	for _, v := range tg.env {
		if strings.HasPrefix(v, "GOROOT=") {
			hasGoroot = true
			break
		}
	}
	prog := tg.goTool()
	if !hasGoroot {
		tg.setenv("GOROOT", testGOROOT)
	}

	tg.t.Logf("running testgo %v", args)
	cmd := exec.Command(prog, args...)
	tg.stdout.Reset()
	tg.stderr.Reset()
	cmd.Dir = tg.execDir
	cmd.Stdout = &tg.stdout
	cmd.Stderr = &tg.stderr
	cmd.Env = tg.env
	status := cmd.Run()
	if tg.stdout.Len() > 0 {
		tg.t.Log("standard output:")
		tg.t.Log(tg.stdout.String())
	}
	if tg.stderr.Len() > 0 {
		tg.t.Log("standard error:")
		tg.t.Log(tg.stderr.String())
	}
	tg.ran = true
	return status
}

// run runs the test go command, and expects it to succeed.
func (tg *testgoData) run(args ...string) {
	tg.t.Helper()
	if status := tg.doRun(args); status != nil {
		wd, _ := os.Getwd()
		tg.t.Logf("go %v failed unexpectedly in %s: %v", args, wd, status)
		tg.t.FailNow()
	}
}

// runFail runs the test go command, and expects it to fail.
func (tg *testgoData) runFail(args ...string) {
	tg.t.Helper()
	if status := tg.doRun(args); status == nil {
		tg.t.Fatal("testgo succeeded unexpectedly")
	} else {
		tg.t.Log("testgo failed as expected:", status)
	}
}

// runGit runs a git command, and expects it to succeed.
func (tg *testgoData) runGit(dir string, args ...string) {
	tg.t.Helper()
	cmd := exec.Command("git", args...)
	tg.stdout.Reset()
	tg.stderr.Reset()
	cmd.Stdout = &tg.stdout
	cmd.Stderr = &tg.stderr
	cmd.Dir = dir
	cmd.Env = tg.env
	status := cmd.Run()
	if tg.stdout.Len() > 0 {
		tg.t.Log("git standard output:")
		tg.t.Log(tg.stdout.String())
	}
	if tg.stderr.Len() > 0 {
		tg.t.Log("git standard error:")
		tg.t.Log(tg.stderr.String())
	}
	if status != nil {
		tg.t.Logf("git %v failed unexpectedly: %v", args, status)
		tg.t.FailNow()
	}
}

// getStdout returns standard output of the testgo run as a string.
func (tg *testgoData) getStdout() string {
	tg.t.Helper()
	if !tg.ran {
		tg.t.Fatal("internal testsuite error: stdout called before run")
	}
	return tg.stdout.String()
}

// getStderr returns standard error of the testgo run as a string.
func (tg *testgoData) getStderr() string {
	tg.t.Helper()
	if !tg.ran {
		tg.t.Fatal("internal testsuite error: stdout called before run")
	}
	return tg.stderr.String()
}

// doGrepMatch looks for a regular expression in a buffer, and returns
// whether it is found. The regular expression is matched against
// each line separately, as with the grep command.
func (tg *testgoData) doGrepMatch(match string, b *bytes.Buffer) bool {
	tg.t.Helper()
	if !tg.ran {
		tg.t.Fatal("internal testsuite error: grep called before run")
	}
	re := regexp.MustCompile(match)
	for _, ln := range bytes.Split(b.Bytes(), []byte{'\n'}) {
		if re.Match(ln) {
			return true
		}
	}
	return false
}

// doGrep looks for a regular expression in a buffer and fails if it
// is not found. The name argument is the name of the output we are
// searching, "output" or "error". The msg argument is logged on
// failure.
func (tg *testgoData) doGrep(match string, b *bytes.Buffer, name, msg string) {
	tg.t.Helper()
	if !tg.doGrepMatch(match, b) {
		tg.t.Log(msg)
		tg.t.Logf("pattern %v not found in standard %s", match, name)
		tg.t.FailNow()
	}
}

// grepStdout looks for a regular expression in the test run's
// standard output and fails, logging msg, if it is not found.
func (tg *testgoData) grepStdout(match, msg string) {
	tg.t.Helper()
	tg.doGrep(match, &tg.stdout, "output", msg)
}

// grepStderr looks for a regular expression in the test run's
// standard error and fails, logging msg, if it is not found.
func (tg *testgoData) grepStderr(match, msg string) {
	tg.t.Helper()
	tg.doGrep(match, &tg.stderr, "error", msg)
}

// grepBoth looks for a regular expression in the test run's standard
// output or stand error and fails, logging msg, if it is not found.
func (tg *testgoData) grepBoth(match, msg string) {
	tg.t.Helper()
	if !tg.doGrepMatch(match, &tg.stdout) && !tg.doGrepMatch(match, &tg.stderr) {
		tg.t.Log(msg)
		tg.t.Logf("pattern %v not found in standard output or standard error", match)
		tg.t.FailNow()
	}
}

// doGrepNot looks for a regular expression in a buffer and fails if
// it is found. The name and msg arguments are as for doGrep.
func (tg *testgoData) doGrepNot(match string, b *bytes.Buffer, name, msg string) {
	tg.t.Helper()
	if tg.doGrepMatch(match, b) {
		tg.t.Log(msg)
		tg.t.Logf("pattern %v found unexpectedly in standard %s", match, name)
		tg.t.FailNow()
	}
}

// grepStdoutNot looks for a regular expression in the test run's
// standard output and fails, logging msg, if it is found.
func (tg *testgoData) grepStdoutNot(match, msg string) {
	tg.t.Helper()
	tg.doGrepNot(match, &tg.stdout, "output", msg)
}

// grepStderrNot looks for a regular expression in the test run's
// standard error and fails, logging msg, if it is found.
func (tg *testgoData) grepStderrNot(match, msg string) {
	tg.t.Helper()
	tg.doGrepNot(match, &tg.stderr, "error", msg)
}

// grepBothNot looks for a regular expression in the test run's
// standard output or standard error and fails, logging msg, if it is
// found.
func (tg *testgoData) grepBothNot(match, msg string) {
	tg.t.Helper()
	if tg.doGrepMatch(match, &tg.stdout) || tg.doGrepMatch(match, &tg.stderr) {
		tg.t.Log(msg)
		tg.t.Fatalf("pattern %v found unexpectedly in standard output or standard error", match)
	}
}

// doGrepCount counts the number of times a regexp is seen in a buffer.
func (tg *testgoData) doGrepCount(match string, b *bytes.Buffer) int {
	tg.t.Helper()
	if !tg.ran {
		tg.t.Fatal("internal testsuite error: doGrepCount called before run")
	}
	re := regexp.MustCompile(match)
	c := 0
	for _, ln := range bytes.Split(b.Bytes(), []byte{'\n'}) {
		if re.Match(ln) {
			c++
		}
	}
	return c
}

// grepCountBoth returns the number of times a regexp is seen in both
// standard output and standard error.
func (tg *testgoData) grepCountBoth(match string) int {
	tg.t.Helper()
	return tg.doGrepCount(match, &tg.stdout) + tg.doGrepCount(match, &tg.stderr)
}

// creatingTemp records that the test plans to create a temporary file
// or directory. If the file or directory exists already, it will be
// removed. When the test completes, the file or directory will be
// removed if it exists.
func (tg *testgoData) creatingTemp(path string) {
	tg.t.Helper()
	if filepath.IsAbs(path) && !strings.HasPrefix(path, tg.tempdir) {
		tg.t.Fatalf("internal testsuite error: creatingTemp(%q) with absolute path not in temporary directory", path)
	}
	// If we have changed the working directory, make sure we have
	// an absolute path, because we are going to change directory
	// back before we remove the temporary.
	if !filepath.IsAbs(path) {
		if tg.wd == "" || strings.HasPrefix(tg.wd, testGOROOT) {
			tg.t.Fatalf("internal testsuite error: creatingTemp(%q) within GOROOT/src", path)
		}
		path = filepath.Join(tg.wd, path)
	}
	tg.must(robustio.RemoveAll(path))
	tg.temps = append(tg.temps, path)
}

// makeTempdir makes a temporary directory for a run of testgo. If
// the temporary directory was already created, this does nothing.
func (tg *testgoData) makeTempdir() {
	tg.t.Helper()
	if tg.tempdir == "" {
		var err error
		tg.tempdir, err = ioutil.TempDir("", "gotest")
		tg.must(err)
	}
}

// tempFile adds a temporary file for a run of testgo.
func (tg *testgoData) tempFile(path, contents string) {
	tg.t.Helper()
	tg.makeTempdir()
	tg.must(os.MkdirAll(filepath.Join(tg.tempdir, filepath.Dir(path)), 0755))
	bytes := []byte(contents)
	if strings.HasSuffix(path, ".go") {
		formatted, err := format.Source(bytes)
		if err == nil {
			bytes = formatted
		}
	}
	tg.must(ioutil.WriteFile(filepath.Join(tg.tempdir, path), bytes, 0644))
}

// tempDir adds a temporary directory for a run of testgo.
func (tg *testgoData) tempDir(path string) {
	tg.t.Helper()
	tg.makeTempdir()
	if err := os.MkdirAll(filepath.Join(tg.tempdir, path), 0755); err != nil && !os.IsExist(err) {
		tg.t.Fatal(err)
	}
}

// path returns the absolute pathname to file with the temporary
// directory.
func (tg *testgoData) path(name string) string {
	tg.t.Helper()
	if tg.tempdir == "" {
		tg.t.Fatalf("internal testsuite error: path(%q) with no tempdir", name)
	}
	if name == "." {
		return tg.tempdir
	}
	return filepath.Join(tg.tempdir, name)
}

// mustExist fails if path does not exist.
func (tg *testgoData) mustExist(path string) {
	tg.t.Helper()
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			tg.t.Fatalf("%s does not exist but should", path)
		}
		tg.t.Fatalf("%s stat failed: %v", path, err)
	}
}

// mustNotExist fails if path exists.
func (tg *testgoData) mustNotExist(path string) {
	tg.t.Helper()
	if _, err := os.Stat(path); err == nil || !os.IsNotExist(err) {
		tg.t.Fatalf("%s exists but should not (%v)", path, err)
	}
}

// mustHaveContent succeeds if filePath is a path to a file,
// and that file is readable and not empty.
func (tg *testgoData) mustHaveContent(filePath string) {
	tg.mustExist(filePath)
	f, err := os.Stat(filePath)
	if err != nil {
		tg.t.Fatal(err)
	}
	if f.Size() == 0 {
		tg.t.Fatalf("expected %s to have data, but is empty", filePath)
	}
}

// wantExecutable fails with msg if path is not executable.
func (tg *testgoData) wantExecutable(path, msg string) {
	tg.t.Helper()
	if st, err := os.Stat(path); err != nil {
		if !os.IsNotExist(err) {
			tg.t.Log(err)
		}
		tg.t.Fatal(msg)
	} else {
		if runtime.GOOS != "windows" && st.Mode()&0111 == 0 {
			tg.t.Fatalf("binary %s exists but is not executable", path)
		}
	}
}

// wantArchive fails if path is not an archive.
func (tg *testgoData) wantArchive(path string) {
	tg.t.Helper()
	f, err := os.Open(path)
	if err != nil {
		tg.t.Fatal(err)
	}
	buf := make([]byte, 100)
	io.ReadFull(f, buf)
	f.Close()
	if !bytes.HasPrefix(buf, []byte("!<arch>\n")) {
		tg.t.Fatalf("file %s exists but is not an archive", path)
	}
}

// isStale reports whether pkg is stale, and why
func (tg *testgoData) isStale(pkg string) (bool, string) {
	tg.t.Helper()
	tg.run("list", "-f", "{{.Stale}}:{{.StaleReason}}", pkg)
	v := strings.TrimSpace(tg.getStdout())
	f := strings.SplitN(v, ":", 2)
	if len(f) == 2 {
		switch f[0] {
		case "true":
			return true, f[1]
		case "false":
			return false, f[1]
		}
	}
	tg.t.Fatalf("unexpected output checking staleness of package %v: %v", pkg, v)
	panic("unreachable")
}

// wantStale fails with msg if pkg is not stale.
func (tg *testgoData) wantStale(pkg, reason, msg string) {
	tg.t.Helper()
	stale, why := tg.isStale(pkg)
	if !stale {
		tg.t.Fatal(msg)
	}
	// We always accept the reason as being "not installed but
	// available in build cache", because when that is the case go
	// list doesn't try to sort out the underlying reason why the
	// package is not installed.
	if reason == "" && why != "" || !strings.Contains(why, reason) && !strings.Contains(why, "not installed but available in build cache") {
		tg.t.Errorf("wrong reason for Stale=true: %q, want %q", why, reason)
	}
}

// wantNotStale fails with msg if pkg is stale.
func (tg *testgoData) wantNotStale(pkg, reason, msg string) {
	tg.t.Helper()
	stale, why := tg.isStale(pkg)
	if stale {
		tg.t.Fatal(msg)
	}
	if reason == "" && why != "" || !strings.Contains(why, reason) {
		tg.t.Errorf("wrong reason for Stale=false: %q, want %q", why, reason)
	}
}

// If -testwork is specified, the test prints the name of the temp directory
// and does not remove it when done, so that a programmer can
// poke at the test file tree afterward.
var testWork = flag.Bool("testwork", false, "")

// cleanup cleans up a test that runs testgo.
func (tg *testgoData) cleanup() {
	tg.t.Helper()
	if tg.wd != "" {
		wd, _ := os.Getwd()
		tg.t.Logf("ended in %s", wd)

		if err := os.Chdir(tg.wd); err != nil {
			// We are unlikely to be able to continue.
			fmt.Fprintln(os.Stderr, "could not restore working directory, crashing:", err)
			os.Exit(2)
		}
	}
	if *testWork {
		tg.t.Logf("TESTWORK=%s\n", tg.path("."))
		return
	}
	for _, path := range tg.temps {
		tg.check(removeAll(path))
	}
	if tg.tempdir != "" {
		tg.check(removeAll(tg.tempdir))
	}
}

func removeAll(dir string) error {
	// module cache has 0444 directories;
	// make them writable in order to remove content.
	filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // ignore errors walking in file system
		}
		if info.IsDir() {
			os.Chmod(path, 0777)
		}
		return nil
	})
	return robustio.RemoveAll(dir)
}

// failSSH puts an ssh executable in the PATH that always fails.
// This is to stub out uses of ssh by go get.
func (tg *testgoData) failSSH() {
	tg.t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		tg.t.Fatal(err)
	}
	fail := filepath.Join(wd, "testdata/failssh")
	tg.setenv("PATH", fmt.Sprintf("%v%c%v", fail, filepath.ListSeparator, os.Getenv("PATH")))
}

func TestNewReleaseRebuildsStalePackagesInGOPATH(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping lengthy test in short mode")
	}

	tg := testgo(t)
	defer tg.cleanup()
	tg.parallel()

	// Copy the runtime packages into a temporary GOROOT
	// so that we can change files.
	for _, copydir := range []string{
		"src/runtime",
		"src/internal/bytealg",
		"src/internal/cpu",
		"src/math/bits",
		"src/unsafe",
		filepath.Join("pkg", runtime.GOOS+"_"+runtime.GOARCH),
		filepath.Join("pkg/tool", runtime.GOOS+"_"+runtime.GOARCH),
		"pkg/include",
	} {
		srcdir := filepath.Join(testGOROOT, copydir)
		tg.tempDir(filepath.Join("goroot", copydir))
		err := filepath.Walk(srcdir,
			func(path string, info os.FileInfo, err error) error {
				if err != nil {
					return err
				}
				if info.IsDir() {
					return nil
				}
				srcrel, err := filepath.Rel(srcdir, path)
				if err != nil {
					return err
				}
				dest := filepath.Join("goroot", copydir, srcrel)
				data, err := ioutil.ReadFile(path)
				if err != nil {
					return err
				}
				tg.tempFile(dest, string(data))
				if err := os.Chmod(tg.path(dest), info.Mode()|0200); err != nil {
					return err
				}
				return nil
			})
		if err != nil {
			t.Fatal(err)
		}
	}
	tg.setenv("GOROOT", tg.path("goroot"))

	addVar := func(name string, idx int) (restore func()) {
		data, err := ioutil.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		old := data
		data = append(data, fmt.Sprintf("var DummyUnusedVar%d bool\n", idx)...)
		if err := ioutil.WriteFile(name, append(data, '\n'), 0666); err != nil {
			t.Fatal(err)
		}
		tg.sleep()
		return func() {
			if err := ioutil.WriteFile(name, old, 0666); err != nil {
				t.Fatal(err)
			}
		}
	}

	// Every main package depends on the "runtime".
	tg.tempFile("d1/src/p1/p1.go", `package main; func main(){}`)
	tg.setenv("GOPATH", tg.path("d1"))
	// Pass -i flag to rebuild everything outdated.
	tg.run("install", "-i", "p1")
	tg.wantNotStale("p1", "", "./testgo list claims p1 is stale, incorrectly, before any changes")

	// Changing mtime of runtime/internal/sys/sys.go
	// should have no effect: only the content matters.
	// In fact this should be true even outside a release branch.
	sys := tg.path("goroot/src/runtime/internal/sys/sys.go")
	tg.sleep()
	restore := addVar(sys, 0)
	restore()
	tg.wantNotStale("p1", "", "./testgo list claims p1 is stale, incorrectly, after updating mtime of runtime/internal/sys/sys.go")

	// But changing content of any file should have an effect.
	// Previously zversion.go was the only one that mattered;
	// now they all matter, so keep using sys.go.
	restore = addVar(sys, 1)
	defer restore()
	tg.wantStale("p1", "stale dependency: runtime/internal/sys", "./testgo list claims p1 is NOT stale, incorrectly, after changing sys.go")
	restore()
	tg.wantNotStale("p1", "", "./testgo list claims p1 is stale, incorrectly, after changing back to old release")
	addVar(sys, 2)
	tg.wantStale("p1", "stale dependency: runtime", "./testgo list claims p1 is NOT stale, incorrectly, after changing sys.go again")
	tg.run("install", "-i", "p1")
	tg.wantNotStale("p1", "", "./testgo list claims p1 is stale after building with new release")

	// Restore to "old" release.
	restore()
	tg.wantStale("p1", "stale dependency: runtime/internal/sys", "./testgo list claims p1 is NOT stale, incorrectly, after restoring sys.go")
	tg.run("install", "-i", "p1")
	tg.wantNotStale("p1", "", "./testgo list claims p1 is stale after building with old release")
}

// cmd/go: custom import path checking should not apply to Go packages without import comment.
func TestIssue10952(t *testing.T) {
	testenv.MustHaveExternalNetwork(t)
	testenv.MustHaveExecPath(t, "git")

	tg := testgo(t)
	defer tg.cleanup()
	tg.parallel()
	tg.tempDir("src")
	tg.setenv("GOPATH", tg.path("."))
	const importPath = "github.com/zombiezen/go-get-issue-10952"
	tg.run("get", "-d", "-u", importPath)
	repoDir := tg.path("src/" + importPath)
	tg.runGit(repoDir, "remote", "set-url", "origin", "https://"+importPath+".git")
	tg.run("get", "-d", "-u", importPath)
}

func TestIssue16471(t *testing.T) {
	testenv.MustHaveExternalNetwork(t)
	testenv.MustHaveExecPath(t, "git")

	tg := testgo(t)
	defer tg.cleanup()
	tg.parallel()
	tg.tempDir("src")
	tg.setenv("GOPATH", tg.path("."))
	tg.must(os.MkdirAll(tg.path("src/rsc.io/go-get-issue-10952"), 0755))
	tg.runGit(tg.path("src/rsc.io"), "clone", "https://github.com/zombiezen/go-get-issue-10952")
	tg.runFail("get", "-u", "rsc.io/go-get-issue-10952")
	tg.grepStderr("rsc.io/go-get-issue-10952 is a custom import path for https://github.com/rsc/go-get-issue-10952, but .* is checked out from https://github.com/zombiezen/go-get-issue-10952", "did not detect updated import path")
}

// Test git clone URL that uses SCP-like syntax and custom import path checking.
func TestIssue11457(t *testing.T) {
	testenv.MustHaveExternalNetwork(t)
	testenv.MustHaveExecPath(t, "git")

	tg := testgo(t)
	defer tg.cleanup()
	tg.parallel()
	tg.tempDir("src")
	tg.setenv("GOPATH", tg.path("."))
	const importPath = "rsc.io/go-get-issue-11457"
	tg.run("get", "-d", "-u", importPath)
	repoDir := tg.path("src/" + importPath)
	tg.runGit(repoDir, "remote", "set-url", "origin", "git@github.com:rsc/go-get-issue-11457")

	// At this time, custom import path checking compares remotes verbatim (rather than
	// just the host and path, skipping scheme and user), so we expect go get -u to fail.
	// However, the goal of this test is to verify that gitRemoteRepo correctly parsed
	// the SCP-like syntax, and we expect it to appear in the error message.
	tg.runFail("get", "-d", "-u", importPath)
	want := " is checked out from ssh://git@github.com/rsc/go-get-issue-11457"
	if !strings.HasSuffix(strings.TrimSpace(tg.getStderr()), want) {
		t.Error("expected clone URL to appear in stderr")
	}
}

func TestGetGitDefaultBranch(t *testing.T) {
	testenv.MustHaveExternalNetwork(t)
	testenv.MustHaveExecPath(t, "git")

	tg := testgo(t)
	defer tg.cleanup()
	tg.parallel()
	tg.tempDir("src")
	tg.setenv("GOPATH", tg.path("."))

	// This repo has two branches, master and another-branch.
	// The another-branch is the default that you get from 'git clone'.
	// The go get command variants should not override this.
	const importPath = "github.com/rsc/go-get-default-branch"

	tg.run("get", "-d", importPath)
	repoDir := tg.path("src/" + importPath)
	tg.runGit(repoDir, "branch", "--contains", "HEAD")
	tg.grepStdout(`\* another-branch`, "not on correct default branch")

	tg.run("get", "-d", "-u", importPath)
	tg.runGit(repoDir, "branch", "--contains", "HEAD")
	tg.grepStdout(`\* another-branch`, "not on correct default branch")
}

// Security issue. Don't disable. See golang.org/issue/22125.
func TestAccidentalGitCheckout(t *testing.T) {
	testenv.MustHaveExternalNetwork(t)
	testenv.MustHaveExecPath(t, "git")

	tg := testgo(t)
	defer tg.cleanup()
	tg.parallel()
	tg.tempDir("src")

	tg.setenv("GOPATH", tg.path("."))

	tg.runFail("get", "-u", "vcs-test.golang.org/go/test1-svn-git")
	tg.grepStderr("src[\\\\/]vcs-test.* uses git, but parent .*src[\\\\/]vcs-test.* uses svn", "get did not fail for right reason")

	if _, err := os.Stat(tg.path("SrC")); err == nil {
		// This case only triggers on a case-insensitive file system.
		tg.runFail("get", "-u", "vcs-test.golang.org/go/test2-svn-git/test2main")
		tg.grepStderr("src[\\\\/]vcs-test.* uses git, but parent .*src[\\\\/]vcs-test.* uses svn", "get did not fail for right reason")
	}
}

func TestVersionControlErrorMessageIncludesCorrectDirectory(t *testing.T) {
	tg := testgo(t)
	defer tg.cleanup()
	tg.setenv("GOPATH", filepath.Join(tg.pwd(), "testdata/shadow/root1"))
	tg.runFail("get", "-u", "foo")

	// TODO(iant): We should not have to use strconv.Quote here.
	// The code in vcs.go should be changed so that it is not required.
	quoted := strconv.Quote(filepath.Join("testdata", "shadow", "root1", "src", "foo"))
	quoted = quoted[1 : len(quoted)-1]

	tg.grepStderr(regexp.QuoteMeta(quoted), "go get -u error does not mention shadow/root1/src/foo")
}

// Issue 21895
func TestMSanAndRaceRequireCgo(t *testing.T) {
	if !canMSan && !canRace {
		t.Skip("skipping because both msan and the race detector are not supported")
	}

	tg := testgo(t)
	defer tg.cleanup()
	tg.tempFile("triv.go", `package main; func main() {}`)
	tg.setenv("CGO_ENABLED", "0")
	if canRace {
		tg.runFail("install", "-race", "triv.go")
		tg.grepStderr("-race requires cgo", "did not correctly report that -race requires cgo")
		tg.grepStderrNot("-msan", "reported that -msan instead of -race requires cgo")
	}
	if canMSan {
		tg.runFail("install", "-msan", "triv.go")
		tg.grepStderr("-msan requires cgo", "did not correctly report that -msan requires cgo")
		tg.grepStderrNot("-race", "reported that -race instead of -msan requires cgo")
	}
}

func TestRelativeGOBINFail(t *testing.T) {
	tg := testgo(t)
	defer tg.cleanup()
	tg.tempFile("triv.go", `package main; func main() {}`)
	tg.setenv("GOBIN", ".")
	tg.cd(tg.path("."))
	tg.runFail("install")
	tg.grepStderr("cannot install, GOBIN must be an absolute path", "go install must fail if $GOBIN is a relative path")
}

func TestPackageMainTestCompilerFlags(t *testing.T) {
	tg := testgo(t)
	defer tg.cleanup()
	tg.parallel()
	tg.makeTempdir()
	tg.setenv("GOPATH", tg.path("."))
	tg.tempFile("src/p1/p1.go", "package main\n")
	tg.tempFile("src/p1/p1_test.go", "package main\nimport \"testing\"\nfunc Test(t *testing.T){}\n")
	tg.run("test", "-c", "-n", "p1")
	tg.grepBothNot(`([\\/]compile|gccgo).* (-p main|-fgo-pkgpath=main).*p1\.go`, "should not have run compile -p main p1.go")
	tg.grepStderr(`([\\/]compile|gccgo).* (-p p1|-fgo-pkgpath=p1).*p1\.go`, "should have run compile -p p1 p1.go")
}

// Issue 12690
func TestPackageNotStaleWithTrailingSlash(t *testing.T) {
	skipIfGccgo(t, "gccgo does not have GOROOT")
	tg := testgo(t)
	defer tg.cleanup()

	// Make sure the packages below are not stale.
	tg.wantNotStale("runtime", "", "must be non-stale before test runs")
	tg.wantNotStale("os", "", "must be non-stale before test runs")
	tg.wantNotStale("io", "", "must be non-stale before test runs")

	goroot := runtime.GOROOT()
	tg.setenv("GOROOT", goroot+"/")

	tg.wantNotStale("runtime", "", "with trailing slash in GOROOT, runtime listed as stale")
	tg.wantNotStale("os", "", "with trailing slash in GOROOT, os listed as stale")
	tg.wantNotStale("io", "", "with trailing slash in GOROOT, io listed as stale")
}

// Issue 4104.
func TestGoTestWithPackageListedMultipleTimes(t *testing.T) {
	tooSlow(t)
	tg := testgo(t)
	defer tg.cleanup()
	tg.parallel()
	tg.run("test", "errors", "errors", "errors", "errors", "errors")
	if strings.Contains(strings.TrimSpace(tg.getStdout()), "\n") {
		t.Error("go test errors errors errors errors errors tested the same package multiple times")
	}
}

func TestGoListHasAConsistentOrder(t *testing.T) {
	tooSlow(t)
	tg := testgo(t)
	defer tg.cleanup()
	tg.parallel()
	tg.run("list", "std")
	first := tg.getStdout()
	tg.run("list", "std")
	if first != tg.getStdout() {
		t.Error("go list std ordering is inconsistent")
	}
}

func TestGoListStdDoesNotIncludeCommands(t *testing.T) {
	tooSlow(t)
	tg := testgo(t)
	defer tg.cleanup()
	tg.parallel()
	tg.run("list", "std")
	tg.grepStdoutNot("cmd/", "go list std shows commands")
}

func TestGoListCmdOnlyShowsCommands(t *testing.T) {
	skipIfGccgo(t, "gccgo does not have GOROOT")
	tooSlow(t)
	tg := testgo(t)
	defer tg.cleanup()
	tg.parallel()
	tg.run("list", "cmd")
	out := strings.TrimSpace(tg.getStdout())
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(line, "cmd/") {
			t.Error("go list cmd shows non-commands")
			break
		}
	}
}

func TestGoListDeps(t *testing.T) {
	tg := testgo(t)
	defer tg.cleanup()
	tg.parallel()
	tg.tempDir("src/p1/p2/p3/p4")
	tg.setenv("GOPATH", tg.path("."))
	tg.tempFile("src/p1/p.go", "package p1\nimport _ \"p1/p2\"\n")
	tg.tempFile("src/p1/p2/p.go", "package p2\nimport _ \"p1/p2/p3\"\n")
	tg.tempFile("src/p1/p2/p3/p.go", "package p3\nimport _ \"p1/p2/p3/p4\"\n")
	tg.tempFile("src/p1/p2/p3/p4/p.go", "package p4\n")
	tg.run("list", "-f", "{{.Deps}}", "p1")
	tg.grepStdout("p1/p2/p3/p4", "Deps(p1) does not mention p4")

	tg.run("list", "-deps", "p1")
	tg.grepStdout("p1/p2/p3/p4", "-deps p1 does not mention p4")

	if runtime.Compiler != "gccgo" {
		// Check the list is in dependency order.
		tg.run("list", "-deps", "math")
		want := "internal/cpu\nunsafe\nmath/bits\nmath\n"
		out := tg.stdout.String()
		if !strings.Contains(out, "internal/cpu") {
			// Some systems don't use internal/cpu.
			want = "unsafe\nmath/bits\nmath\n"
		}
		if tg.stdout.String() != want {
			t.Fatalf("list -deps math: wrong order\nhave %q\nwant %q", tg.stdout.String(), want)
		}
	}
}

func TestGoListTest(t *testing.T) {
	skipIfGccgo(t, "gccgo does not have standard packages")
	tg := testgo(t)
	defer tg.cleanup()
	tg.parallel()
	tg.makeTempdir()
	tg.setenv("GOCACHE", tg.tempdir)

	tg.run("list", "-test", "-deps", "sort")
	tg.grepStdout(`^sort.test$`, "missing test main")
	tg.grepStdout(`^sort$`, "missing real sort")
	tg.grepStdout(`^sort \[sort.test\]$`, "missing test copy of sort")
	tg.grepStdout(`^testing \[sort.test\]$`, "missing test copy of testing")
	tg.grepStdoutNot(`^testing$`, "unexpected real copy of testing")

	tg.run("list", "-test", "sort")
	tg.grepStdout(`^sort.test$`, "missing test main")
	tg.grepStdout(`^sort$`, "missing real sort")
	tg.grepStdout(`^sort \[sort.test\]$`, "unexpected test copy of sort")
	tg.grepStdoutNot(`^testing \[sort.test\]$`, "unexpected test copy of testing")
	tg.grepStdoutNot(`^testing$`, "unexpected real copy of testing")

	tg.run("list", "-test", "cmd/dist", "cmd/doc")
	tg.grepStdout(`^cmd/dist$`, "missing cmd/dist")
	tg.grepStdout(`^cmd/doc$`, "missing cmd/doc")
	tg.grepStdout(`^cmd/doc\.test$`, "missing cmd/doc test")
	tg.grepStdoutNot(`^cmd/dist\.test$`, "unexpected cmd/dist test")
	tg.grepStdoutNot(`^testing`, "unexpected testing")

	tg.run("list", "-test", "runtime/cgo")
	tg.grepStdout(`^runtime/cgo$`, "missing runtime/cgo")

	tg.run("list", "-deps", "-f", "{{if .DepOnly}}{{.ImportPath}}{{end}}", "sort")
	tg.grepStdout(`^internal/reflectlite$`, "missing internal/reflectlite")
	tg.grepStdoutNot(`^sort`, "unexpected sort")
}

func TestGoListCompiledCgo(t *testing.T) {
	tooSlow(t)
	tg := testgo(t)
	defer tg.cleanup()
	tg.parallel()
	tg.makeTempdir()
	tg.setenv("GOCACHE", tg.tempdir)

	tg.run("list", "-f", `{{join .CgoFiles "\n"}}`, "net")
	if tg.stdout.String() == "" {
		t.Skip("net does not use cgo")
	}
	if strings.Contains(tg.stdout.String(), tg.tempdir) {
		t.Fatalf(".CgoFiles unexpectedly mentioned cache %s", tg.tempdir)
	}
	tg.run("list", "-compiled", "-f", `{{.Dir}}{{"\n"}}{{join .CompiledGoFiles "\n"}}`, "net")
	if !strings.Contains(tg.stdout.String(), tg.tempdir) {
		t.Fatalf(".CompiledGoFiles with -compiled did not mention cache %s", tg.tempdir)
	}
	dir := ""
	for _, file := range strings.Split(tg.stdout.String(), "\n") {
		if file == "" {
			continue
		}
		if dir == "" {
			dir = file
			continue
		}
		if !strings.Contains(file, "/") && !strings.Contains(file, `\`) {
			file = filepath.Join(dir, file)
		}
		if _, err := os.Stat(file); err != nil {
			t.Fatalf("cannot find .CompiledGoFiles result %s: %v", file, err)
		}
	}
}

func TestGoListExport(t *testing.T) {
	skipIfGccgo(t, "gccgo does not have standard packages")
	tg := testgo(t)
	defer tg.cleanup()
	tg.parallel()
	tg.makeTempdir()
	tg.setenv("GOCACHE", tg.tempdir)

	tg.run("list", "-f", "{{.Export}}", "strings")
	if tg.stdout.String() != "" {
		t.Fatalf(".Export without -export unexpectedly set")
	}
	tg.run("list", "-export", "-f", "{{.Export}}", "strings")
	file := strings.TrimSpace(tg.stdout.String())
	if file == "" {
		t.Fatalf(".Export with -export was empty")
	}
	if _, err := os.Stat(file); err != nil {
		t.Fatalf("cannot find .Export result %s: %v", file, err)
	}
}

// Issue 4096. Validate the output of unsuccessful go install foo/quxx.
func TestUnsuccessfulGoInstallShouldMentionMissingPackage(t *testing.T) {
	tg := testgo(t)
	defer tg.cleanup()
	tg.parallel()
	tg.runFail("install", "foo/quxx")
	if tg.grepCountBoth(`cannot find package "foo/quxx" in any of`) != 1 {
		t.Error(`go install foo/quxx expected error: .*cannot find package "foo/quxx" in any of`)
	}
}

func TestGOROOTSearchFailureReporting(t *testing.T) {
	tg := testgo(t)
	defer tg.cleanup()
	tg.parallel()
	tg.runFail("install", "foo/quxx")
	if tg.grepCountBoth(regexp.QuoteMeta(filepath.Join("foo", "quxx"))+` \(from \$GOROOT\)$`) != 1 {
		t.Error(`go install foo/quxx expected error: .*foo/quxx (from $GOROOT)`)
	}
}

func TestMultipleGOPATHEntriesReportedSeparately(t *testing.T) {
	tg := testgo(t)
	defer tg.cleanup()
	tg.parallel()
	sep := string(filepath.ListSeparator)
	tg.setenv("GOPATH", filepath.Join(tg.pwd(), "testdata", "a")+sep+filepath.Join(tg.pwd(), "testdata", "b"))
	tg.runFail("install", "foo/quxx")
	if tg.grepCountBoth(`testdata[/\\].[/\\]src[/\\]foo[/\\]quxx`) != 2 {
		t.Error(`go install foo/quxx expected error: .*testdata/a/src/foo/quxx (from $GOPATH)\n.*testdata/b/src/foo/quxx`)
	}
}

// Test (from $GOPATH) annotation is reported for the first GOPATH entry,
func TestMentionGOPATHInFirstGOPATHEntry(t *testing.T) {
	tg := testgo(t)
	defer tg.cleanup()
	tg.parallel()
	sep := string(filepath.ListSeparator)
	tg.setenv("GOPATH", filepath.Join(tg.pwd(), "testdata", "a")+sep+filepath.Join(tg.pwd(), "testdata", "b"))
	tg.runFail("install", "foo/quxx")
	if tg.grepCountBoth(regexp.QuoteMeta(filepath.Join("testdata", "a", "src", "foo", "quxx"))+` \(from \$GOPATH\)$`) != 1 {
		t.Error(`go install foo/quxx expected error: .*testdata/a/src/foo/quxx (from $GOPATH)`)
	}
}

// but not on the second.
func TestMentionGOPATHNotOnSecondEntry(t *testing.T) {
	tg := testgo(t)
	defer tg.cleanup()
	tg.parallel()
	sep := string(filepath.ListSeparator)
	tg.setenv("GOPATH", filepath.Join(tg.pwd(), "testdata", "a")+sep+filepath.Join(tg.pwd(), "testdata", "b"))
	tg.runFail("install", "foo/quxx")
	if tg.grepCountBoth(regexp.QuoteMeta(filepath.Join("testdata", "b", "src", "foo", "quxx"))+`$`) != 1 {
		t.Error(`go install foo/quxx expected error: .*testdata/b/src/foo/quxx`)
	}
}

func homeEnvName() string {
	switch runtime.GOOS {
	case "windows":
		return "USERPROFILE"
	case "plan9":
		return "home"
	default:
		return "HOME"
	}
}

func tempEnvName() string {
	switch runtime.GOOS {
	case "windows":
		return "TMP"
	case "plan9":
		return "TMPDIR" // actually plan 9 doesn't have one at all but this is fine
	default:
		return "TMPDIR"
	}
}

func TestDefaultGOPATH(t *testing.T) {
	tg := testgo(t)
	defer tg.cleanup()
	tg.parallel()
	tg.tempDir("home/go")
	tg.setenv(homeEnvName(), tg.path("home"))

	tg.run("env", "GOPATH")
	tg.grepStdout(regexp.QuoteMeta(tg.path("home/go")), "want GOPATH=$HOME/go")

	tg.setenv("GOROOT", tg.path("home/go"))
	tg.run("env", "GOPATH")
	tg.grepStdoutNot(".", "want unset GOPATH because GOROOT=$HOME/go")

	tg.setenv("GOROOT", tg.path("home/go")+"/")
	tg.run("env", "GOPATH")
	tg.grepStdoutNot(".", "want unset GOPATH because GOROOT=$HOME/go/")
}

func TestDefaultGOPATHGet(t *testing.T) {
	testenv.MustHaveExternalNetwork(t)
	testenv.MustHaveExecPath(t, "git")

	tg := testgo(t)
	defer tg.cleanup()
	tg.parallel()
	tg.setenv("GOPATH", "")
	tg.tempDir("home")
	tg.setenv(homeEnvName(), tg.path("home"))

	// warn for creating directory
	tg.run("get", "-v", "github.com/golang/example/hello")
	tg.grepStderr("created GOPATH="+regexp.QuoteMeta(tg.path("home/go"))+"; see 'go help gopath'", "did not create GOPATH")

	// no warning if directory already exists
	tg.must(robustio.RemoveAll(tg.path("home/go")))
	tg.tempDir("home/go")
	tg.run("get", "github.com/golang/example/hello")
	tg.grepStderrNot(".", "expected no output on standard error")

	// error if $HOME/go is a file
	tg.must(robustio.RemoveAll(tg.path("home/go")))
	tg.tempFile("home/go", "")
	tg.runFail("get", "github.com/golang/example/hello")
	tg.grepStderr(`mkdir .*[/\\]go: .*(not a directory|cannot find the path)`, "expected error because $HOME/go is a file")
}

func TestDefaultGOPATHPrintedSearchList(t *testing.T) {
	tg := testgo(t)
	defer tg.cleanup()
	tg.parallel()
	tg.setenv("GOPATH", "")
	tg.tempDir("home")
	tg.setenv(homeEnvName(), tg.path("home"))

	tg.runFail("install", "github.com/golang/example/hello")
	tg.grepStderr(regexp.QuoteMeta(tg.path("home/go/src/github.com/golang/example/hello"))+`.*from \$GOPATH`, "expected default GOPATH")
}

func TestLdflagsArgumentsWithSpacesIssue3941(t *testing.T) {
	skipIfGccgo(t, "gccgo does not support -ldflags -X")
	tooSlow(t)
	tg := testgo(t)
	defer tg.cleanup()
	tg.parallel()
	tg.tempFile("main.go", `package main
		var extern string
		func main() {
			println(extern)
		}`)
	tg.run("run", "-ldflags", `-X "main.extern=hello world"`, tg.path("main.go"))
	tg.grepStderr("^hello world", `ldflags -X "main.extern=hello world"' failed`)
}

func TestGoTestDashCDashOControlsBinaryLocation(t *testing.T) {
	skipIfGccgo(t, "gccgo has no standard packages")
	tooSlow(t)
	tg := testgo(t)
	defer tg.cleanup()
	tg.parallel()
	tg.makeTempdir()
	tg.run("test", "-c", "-o", tg.path("myerrors.test"+exeSuffix), "errors")
	tg.wantExecutable(tg.path("myerrors.test"+exeSuffix), "go test -c -o myerrors.test did not create myerrors.test")
}

func TestGoTestDashOWritesBinary(t *testing.T) {
	skipIfGccgo(t, "gccgo has no standard packages")
	tooSlow(t)
	tg := testgo(t)
	defer tg.cleanup()
	tg.parallel()
	tg.makeTempdir()
	tg.run("test", "-o", tg.path("myerrors.test"+exeSuffix), "errors")
	tg.wantExecutable(tg.path("myerrors.test"+exeSuffix), "go test -o myerrors.test did not create myerrors.test")
}

func TestGoTestDashIDashOWritesBinary(t *testing.T) {
	skipIfGccgo(t, "gccgo has no standard packages")
	tooSlow(t)
	tg := testgo(t)
	defer tg.cleanup()
	tg.parallel()
	tg.makeTempdir()

	// don't let test -i overwrite runtime
	tg.wantNotStale("runtime", "", "must be non-stale before test -i")

	tg.run("test", "-v", "-i", "-o", tg.path("myerrors.test"+exeSuffix), "errors")
	tg.grepBothNot("PASS|FAIL", "test should not have run")
	tg.wantExecutable(tg.path("myerrors.test"+exeSuffix), "go test -o myerrors.test did not create myerrors.test")
}

// Issue 4515.
func TestInstallWithTags(t *testing.T) {
	tooSlow(t)
	tg := testgo(t)
	defer tg.cleanup()
	tg.parallel()
	tg.tempDir("bin")
	tg.tempFile("src/example/a/main.go", `package main
		func main() {}`)
	tg.tempFile("src/example/b/main.go", `// +build mytag

		package main
		func main() {}`)
	tg.setenv("GOPATH", tg.path("."))
	tg.run("install", "-tags", "mytag", "example/a", "example/b")
	tg.wantExecutable(tg.path("bin/a"+exeSuffix), "go install example/a example/b did not install binaries")
	tg.wantExecutable(tg.path("bin/b"+exeSuffix), "go install example/a example/b did not install binaries")
	tg.must(os.Remove(tg.path("bin/a" + exeSuffix)))
	tg.must(os.Remove(tg.path("bin/b" + exeSuffix)))
	tg.run("install", "-tags", "mytag", "example/...")
	tg.wantExecutable(tg.path("bin/a"+exeSuffix), "go install example/... did not install binaries")
	tg.wantExecutable(tg.path("bin/b"+exeSuffix), "go install example/... did not install binaries")
	tg.run("list", "-tags", "mytag", "example/b...")
	if strings.TrimSpace(tg.getStdout()) != "example/b" {
		t.Error("go list example/b did not find example/b")
	}
}

// Issue 4773
func TestCaseCollisions(t *testing.T) {
	tg := testgo(t)
	defer tg.cleanup()
	tg.parallel()
	tg.tempDir("src/example/a/pkg")
	tg.tempDir("src/example/a/Pkg")
	tg.tempDir("src/example/b")
	tg.setenv("GOPATH", tg.path("."))
	tg.tempFile("src/example/a/a.go", `package p
		import (
			_ "example/a/pkg"
			_ "example/a/Pkg"
		)`)
	tg.tempFile("src/example/a/pkg/pkg.go", `package pkg`)
	tg.tempFile("src/example/a/Pkg/pkg.go", `package pkg`)
	tg.run("list", "-json", "example/a")
	tg.grepStdout("case-insensitive import collision", "go list -json example/a did not report import collision")
	tg.runFail("build", "example/a")
	tg.grepStderr("case-insensitive import collision", "go build example/a did not report import collision")
	tg.tempFile("src/example/b/file.go", `package b`)
	tg.tempFile("src/example/b/FILE.go", `package b`)
	f, err := os.Open(tg.path("src/example/b"))
	tg.must(err)
	names, err := f.Readdirnames(0)
	tg.must(err)
	tg.check(f.Close())
	args := []string{"list"}
	if len(names) == 2 {
		// case-sensitive file system, let directory read find both files
		args = append(args, "example/b")
	} else {
		// case-insensitive file system, list files explicitly on command line
		args = append(args, tg.path("src/example/b/file.go"), tg.path("src/example/b/FILE.go"))
	}
	tg.runFail(args...)
	tg.grepStderr("case-insensitive file name collision", "go list example/b did not report file name collision")

	tg.runFail("list", "example/a/pkg", "example/a/Pkg")
	tg.grepStderr("case-insensitive import collision", "go list example/a/pkg example/a/Pkg did not report import collision")
	tg.run("list", "-json", "-e", "example/a/pkg", "example/a/Pkg")
	tg.grepStdout("case-insensitive import collision", "go list -json -e example/a/pkg example/a/Pkg did not report import collision")
	tg.runFail("build", "example/a/pkg", "example/a/Pkg")
	tg.grepStderr("case-insensitive import collision", "go build example/a/pkg example/a/Pkg did not report import collision")
}

// Issue 17451, 17662.
func TestSymlinkWarning(t *testing.T) {
	tg := testgo(t)
	defer tg.cleanup()
	tg.parallel()
	tg.makeTempdir()
	tg.setenv("GOPATH", tg.path("."))

	tg.tempDir("src/example/xx")
	tg.tempDir("yy/zz")
	tg.tempFile("yy/zz/zz.go", "package zz\n")
	if err := os.Symlink(tg.path("yy"), tg.path("src/example/xx/yy")); err != nil {
		t.Skipf("symlink failed: %v", err)
	}
	tg.run("list", "example/xx/z...")
	tg.grepStdoutNot(".", "list should not have matched anything")
	tg.grepStderr("matched no packages", "list should have reported that pattern matched no packages")
	tg.grepStderrNot("symlink", "list should not have reported symlink")

	tg.run("list", "example/xx/...")
	tg.grepStdoutNot(".", "list should not have matched anything")
	tg.grepStderr("matched no packages", "list should have reported that pattern matched no packages")
	tg.grepStderr("ignoring symlink", "list should have reported symlink")
}

func TestShadowingLogic(t *testing.T) {
	skipIfGccgo(t, "gccgo has no standard packages")
	tg := testgo(t)
	defer tg.cleanup()
	pwd := tg.pwd()
	sep := string(filepath.ListSeparator)
	tg.setenv("GOPATH", filepath.Join(pwd, "testdata", "shadow", "root1")+sep+filepath.Join(pwd, "testdata", "shadow", "root2"))

	// The math in root1 is not "math" because the standard math is.
	tg.run("list", "-f", "({{.ImportPath}}) ({{.ConflictDir}})", "./testdata/shadow/root1/src/math")
	pwdForwardSlash := strings.ReplaceAll(pwd, string(os.PathSeparator), "/")
	if !strings.HasPrefix(pwdForwardSlash, "/") {
		pwdForwardSlash = "/" + pwdForwardSlash
	}
	// The output will have makeImportValid applies, but we only
	// bother to deal with characters we might reasonably see.
	for _, r := range " :" {
		pwdForwardSlash = strings.ReplaceAll(pwdForwardSlash, string(r), "_")
	}
	want := "(_" + pwdForwardSlash + "/testdata/shadow/root1/src/math) (" + filepath.Join(runtime.GOROOT(), "src", "math") + ")"
	if strings.TrimSpace(tg.getStdout()) != want {
		t.Error("shadowed math is not shadowed; looking for", want)
	}

	// The foo in root1 is "foo".
	tg.run("list", "-f", "({{.ImportPath}}) ({{.ConflictDir}})", "./testdata/shadow/root1/src/foo")
	if strings.TrimSpace(tg.getStdout()) != "(foo) ()" {
		t.Error("unshadowed foo is shadowed")
	}

	// The foo in root2 is not "foo" because the foo in root1 got there first.
	tg.run("list", "-f", "({{.ImportPath}}) ({{.ConflictDir}})", "./testdata/shadow/root2/src/foo")
	want = "(_" + pwdForwardSlash + "/testdata/shadow/root2/src/foo) (" + filepath.Join(pwd, "testdata", "shadow", "root1", "src", "foo") + ")"
	if strings.TrimSpace(tg.getStdout()) != want {
		t.Error("shadowed foo is not shadowed; looking for", want)
	}

	// The error for go install should mention the conflicting directory.
	tg.runFail("install", "./testdata/shadow/root2/src/foo")
	want = "go install: no install location for " + filepath.Join(pwd, "testdata", "shadow", "root2", "src", "foo") + ": hidden by " + filepath.Join(pwd, "testdata", "shadow", "root1", "src", "foo")
	if strings.TrimSpace(tg.getStderr()) != want {
		t.Error("wrong shadowed install error; looking for", want)
	}
}

// Check that coverage analysis works at all.
// Don't worry about the exact numbers but require not 0.0%.
func checkCoverage(tg *testgoData, data string) {
	tg.t.Helper()
	if regexp.MustCompile(`[^0-9]0\.0%`).MatchString(data) {
		tg.t.Error("some coverage results are 0.0%")
	}
}

func TestCoverageRuns(t *testing.T) {
	skipIfGccgo(t, "gccgo has no cover tool")
	tooSlow(t)
	tg := testgo(t)
	defer tg.cleanup()
	tg.run("test", "-short", "-coverpkg=strings", "strings", "regexp")
	data := tg.getStdout() + tg.getStderr()
	tg.run("test", "-short", "-cover", "strings", "math", "regexp")
	data += tg.getStdout() + tg.getStderr()
	checkCoverage(tg, data)
}

func TestBuildDryRunWithCgo(t *testing.T) {
	if !canCgo {
		t.Skip("skipping because cgo not enabled")
	}

	tg := testgo(t)
	defer tg.cleanup()
	tg.tempFile("foo.go", `package main

/*
#include <limits.h>
*/
import "C"

func main() {
        println(C.INT_MAX)
}`)
	tg.run("build", "-n", tg.path("foo.go"))
	tg.grepStderrNot(`os.Stat .* no such file or directory`, "unexpected stat of archive file")
}

func TestCgoDependsOnSyscall(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test that removes $GOROOT/pkg/*_race in short mode")
	}
	if !canCgo {
		t.Skip("skipping because cgo not enabled")
	}
	if !canRace {
		t.Skip("skipping because race detector not supported")
	}

	tg := testgo(t)
	defer tg.cleanup()
	tg.parallel()

	files, err := filepath.Glob(filepath.Join(runtime.GOROOT(), "pkg", "*_race"))
	tg.must(err)
	for _, file := range files {
		tg.check(robustio.RemoveAll(file))
	}
	tg.tempFile("src/foo/foo.go", `
		package foo
		//#include <stdio.h>
		import "C"`)
	tg.setenv("GOPATH", tg.path("."))
	tg.run("build", "-race", "foo")
}

func TestCgoShowsFullPathNames(t *testing.T) {
	if !canCgo {
		t.Skip("skipping because cgo not enabled")
	}

	tg := testgo(t)
	defer tg.cleanup()
	tg.parallel()
	tg.tempFile("src/x/y/dirname/foo.go", `
		package foo
		import "C"
		func f() {`)
	tg.setenv("GOPATH", tg.path("."))
	tg.runFail("build", "x/y/dirname")
	tg.grepBoth("x/y/dirname", "error did not use full path")
}

func TestCgoHandlesWlORIGIN(t *testing.T) {
	tooSlow(t)
	if !canCgo {
		t.Skip("skipping because cgo not enabled")
	}

	tg := testgo(t)
	defer tg.cleanup()
	tg.parallel()
	tg.tempFile("src/origin/origin.go", `package origin
		// #cgo !darwin LDFLAGS: -Wl,-rpath,$ORIGIN
		// void f(void) {}
		import "C"
		func f() { C.f() }`)
	tg.setenv("GOPATH", tg.path("."))
	tg.run("build", "origin")
}

func TestCgoPkgConfig(t *testing.T) {
	tooSlow(t)
	if !canCgo {
		t.Skip("skipping because cgo not enabled")
	}
	tg := testgo(t)
	defer tg.cleanup()
	tg.parallel()

	tg.run("env", "PKG_CONFIG")
	pkgConfig := strings.TrimSpace(tg.getStdout())
	testenv.MustHaveExecPath(t, pkgConfig)
	if out, err := exec.Command(pkgConfig, "--atleast-pkgconfig-version", "0.24").CombinedOutput(); err != nil {
		t.Skipf("%s --atleast-pkgconfig-version 0.24: %v\n%s", pkgConfig, err, out)
	}

	// OpenBSD's pkg-config is strict about whitespace and only
	// supports backslash-escaped whitespace. It does not support
	// quotes, which the normal freedesktop.org pkg-config does
	// support. See https://man.openbsd.org/pkg-config.1
	tg.tempFile("foo.pc", `
Name: foo
Description: The foo library
Version: 1.0.0
Cflags: -Dhello=10 -Dworld=+32 -DDEFINED_FROM_PKG_CONFIG=hello\ world
`)
	tg.tempFile("foo.go", `package main

/*
#cgo pkg-config: foo
int value() {
	return DEFINED_FROM_PKG_CONFIG;
}
*/
import "C"
import "os"

func main() {
	if C.value() != 42 {
		println("value() =", C.value(), "wanted 42")
		os.Exit(1)
	}
}
`)
	tg.setenv("PKG_CONFIG_PATH", tg.path("."))
	tg.run("run", tg.path("foo.go"))
}

// "go test -c -test.bench=XXX errors" should not hang.
// "go test -c" should also produce reproducible binaries.
// "go test -c" should also appear to write a new binary every time,
// even if it's really just updating the mtime on an existing up-to-date binary.
func TestIssue6480(t *testing.T) {
	skipIfGccgo(t, "gccgo has no standard packages")
	tooSlow(t)
	tg := testgo(t)
	defer tg.cleanup()
	// TODO: tg.parallel()
	tg.makeTempdir()
	tg.cd(tg.path("."))
	tg.run("test", "-c", "-test.bench=XXX", "errors")
	tg.run("test", "-c", "-o", "errors2.test", "errors")

	data1, err := ioutil.ReadFile("errors.test" + exeSuffix)
	tg.must(err)
	data2, err := ioutil.ReadFile("errors2.test") // no exeSuffix because -o above doesn't have it
	tg.must(err)
	if !bytes.Equal(data1, data2) {
		t.Fatalf("go test -c errors produced different binaries when run twice")
	}

	start := time.Now()
	tg.run("test", "-x", "-c", "-test.bench=XXX", "errors")
	tg.grepStderrNot(`[\\/]link|gccgo`, "incorrectly relinked up-to-date test binary")
	info, err := os.Stat("errors.test" + exeSuffix)
	if err != nil {
		t.Fatal(err)
	}
	start = truncateLike(start, info.ModTime())
	if info.ModTime().Before(start) {
		t.Fatalf("mtime of errors.test predates test -c command (%v < %v)", info.ModTime(), start)
	}

	start = time.Now()
	tg.run("test", "-x", "-c", "-o", "errors2.test", "errors")
	tg.grepStderrNot(`[\\/]link|gccgo`, "incorrectly relinked up-to-date test binary")
	info, err = os.Stat("errors2.test")
	if err != nil {
		t.Fatal(err)
	}
	start = truncateLike(start, info.ModTime())
	if info.ModTime().Before(start) {
		t.Fatalf("mtime of errors2.test predates test -c command (%v < %v)", info.ModTime(), start)
	}
}

// truncateLike returns the result of truncating t to the apparent precision of p.
func truncateLike(t, p time.Time) time.Time {
	nano := p.UnixNano()
	d := 1 * time.Nanosecond
	for nano%int64(d) == 0 && d < 1*time.Second {
		d *= 10
	}
	for nano%int64(d) == 0 && d < 2*time.Second {
		d *= 2
	}
	return t.Truncate(d)
}

// cmd/cgo: undefined reference when linking a C-library using gccgo
func TestIssue7573(t *testing.T) {
	if !canCgo {
		t.Skip("skipping because cgo not enabled")
	}
	testenv.MustHaveExecPath(t, "gccgo")

	tg := testgo(t)
	defer tg.cleanup()
	tg.parallel()
	tg.tempFile("src/cgoref/cgoref.go", `
package main
// #cgo LDFLAGS: -L alibpath -lalib
// void f(void) {}
import "C"

func main() { C.f() }`)
	tg.setenv("GOPATH", tg.path("."))
	tg.run("build", "-n", "-compiler", "gccgo", "cgoref")
	tg.grepStderr(`gccgo.*\-L [^ ]*alibpath \-lalib`, `no Go-inline "#cgo LDFLAGS:" ("-L alibpath -lalib") passed to gccgo linking stage`)
}

func TestListTemplateContextFunction(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		v    string
		want string
	}{
		{"GOARCH", runtime.GOARCH},
		{"GOOS", runtime.GOOS},
		{"GOROOT", filepath.Clean(runtime.GOROOT())},
		{"GOPATH", os.Getenv("GOPATH")},
		{"CgoEnabled", ""},
		{"UseAllFiles", ""},
		{"Compiler", ""},
		{"BuildTags", ""},
		{"ReleaseTags", ""},
		{"InstallSuffix", ""},
	} {
		tt := tt
		t.Run(tt.v, func(t *testing.T) {
			tg := testgo(t)
			tg.parallel()
			defer tg.cleanup()
			tmpl := "{{context." + tt.v + "}}"
			tg.run("list", "-f", tmpl)
			if tt.want == "" {
				return
			}
			if got := strings.TrimSpace(tg.getStdout()); got != tt.want {
				t.Errorf("go list -f %q: got %q; want %q", tmpl, got, tt.want)
			}
		})
	}
}

func TestGoTestBuildsAnXtestContainingOnlyNonRunnableExamples(t *testing.T) {
	tg := testgo(t)
	defer tg.cleanup()
	tg.run("test", "-v", "./testdata/norunexample")
	tg.grepStdout("File with non-runnable example was built.", "file with non-runnable example was not built")
}

// Test that you cannot use a local import in a package
// accessed by a non-local import (found in a GOPATH/GOROOT).
// See golang.org/issue/17475.
func TestImportLocal(t *testing.T) {
	tooSlow(t)

	tg := testgo(t)
	tg.parallel()
	defer tg.cleanup()

	tg.tempFile("src/dir/x/x.go", `package x
		var X int
	`)
	tg.setenv("GOPATH", tg.path("."))
	tg.run("build", "dir/x")

	// Ordinary import should work.
	tg.tempFile("src/dir/p0/p.go", `package p0
		import "dir/x"
		var _ = x.X
	`)
	tg.run("build", "dir/p0")

	// Relative import should not.
	tg.tempFile("src/dir/p1/p.go", `package p1
		import "../x"
		var _ = x.X
	`)
	tg.runFail("build", "dir/p1")
	tg.grepStderr("local import.*in non-local package", "did not diagnose local import")

	// ... even in a test.
	tg.tempFile("src/dir/p2/p.go", `package p2
	`)
	tg.tempFile("src/dir/p2/p_test.go", `package p2
		import "../x"
		import "testing"
		var _ = x.X
		func TestFoo(t *testing.T) {}
	`)
	tg.run("build", "dir/p2")
	tg.runFail("test", "dir/p2")
	tg.grepStderr("local import.*in non-local package", "did not diagnose local import")

	// ... even in an xtest.
	tg.tempFile("src/dir/p2/p_test.go", `package p2_test
		import "../x"
		import "testing"
		var _ = x.X
		func TestFoo(t *testing.T) {}
	`)
	tg.run("build", "dir/p2")
	tg.runFail("test", "dir/p2")
	tg.grepStderr("local import.*in non-local package", "did not diagnose local import")

	// Relative import starting with ./ should not work either.
	tg.tempFile("src/dir/d.go", `package dir
		import "./x"
		var _ = x.X
	`)
	tg.runFail("build", "dir")
	tg.grepStderr("local import.*in non-local package", "did not diagnose local import")

	// ... even in a test.
	tg.tempFile("src/dir/d.go", `package dir
	`)
	tg.tempFile("src/dir/d_test.go", `package dir
		import "./x"
		import "testing"
		var _ = x.X
		func TestFoo(t *testing.T) {}
	`)
	tg.run("build", "dir")
	tg.runFail("test", "dir")
	tg.grepStderr("local import.*in non-local package", "did not diagnose local import")

	// ... even in an xtest.
	tg.tempFile("src/dir/d_test.go", `package dir_test
		import "./x"
		import "testing"
		var _ = x.X
		func TestFoo(t *testing.T) {}
	`)
	tg.run("build", "dir")
	tg.runFail("test", "dir")
	tg.grepStderr("local import.*in non-local package", "did not diagnose local import")

	// Relative import plain ".." should not work.
	tg.tempFile("src/dir/x/y/y.go", `package dir
		import ".."
		var _ = x.X
	`)
	tg.runFail("build", "dir/x/y")
	tg.grepStderr("local import.*in non-local package", "did not diagnose local import")

	// ... even in a test.
	tg.tempFile("src/dir/x/y/y.go", `package y
	`)
	tg.tempFile("src/dir/x/y/y_test.go", `package y
		import ".."
		import "testing"
		var _ = x.X
		func TestFoo(t *testing.T) {}
	`)
	tg.run("build", "dir/x/y")
	tg.runFail("test", "dir/x/y")
	tg.grepStderr("local import.*in non-local package", "did not diagnose local import")

	// ... even in an x test.
	tg.tempFile("src/dir/x/y/y_test.go", `package y_test
		import ".."
		import "testing"
		var _ = x.X
		func TestFoo(t *testing.T) {}
	`)
	tg.run("build", "dir/x/y")
	tg.runFail("test", "dir/x/y")
	tg.grepStderr("local import.*in non-local package", "did not diagnose local import")

	// Relative import "." should not work.
	tg.tempFile("src/dir/x/xx.go", `package x
		import "."
		var _ = x.X
	`)
	tg.runFail("build", "dir/x")
	tg.grepStderr("cannot import current directory", "did not diagnose import current directory")

	// ... even in a test.
	tg.tempFile("src/dir/x/xx.go", `package x
	`)
	tg.tempFile("src/dir/x/xx_test.go", `package x
		import "."
		import "testing"
		var _ = x.X
		func TestFoo(t *testing.T) {}
	`)
	tg.run("build", "dir/x")
	tg.runFail("test", "dir/x")
	tg.grepStderr("cannot import current directory", "did not diagnose import current directory")

	// ... even in an xtest.
	tg.tempFile("src/dir/x/xx.go", `package x
	`)
	tg.tempFile("src/dir/x/xx_test.go", `package x_test
		import "."
		import "testing"
		var _ = x.X
		func TestFoo(t *testing.T) {}
	`)
	tg.run("build", "dir/x")
	tg.runFail("test", "dir/x")
	tg.grepStderr("cannot import current directory", "did not diagnose import current directory")
}

func TestGoRunDirs(t *testing.T) {
	tg := testgo(t)
	defer tg.cleanup()
	tg.cd("testdata/rundir")
	tg.runFail("run", "x.go", "sub/sub.go")
	tg.grepStderr("named files must all be in one directory; have ./ and sub/", "wrong output")
	tg.runFail("run", "sub/sub.go", "x.go")
	tg.grepStderr("named files must all be in one directory; have sub/ and ./", "wrong output")
}

func TestGoInstallPkgdir(t *testing.T) {
	skipIfGccgo(t, "gccgo has no standard packages")
	tooSlow(t)

	tg := testgo(t)
	tg.parallel()
	defer tg.cleanup()
	tg.makeTempdir()
	pkg := tg.path(".")
	tg.run("install", "-pkgdir", pkg, "sync")
	tg.mustExist(filepath.Join(pkg, "sync.a"))
	tg.mustNotExist(filepath.Join(pkg, "sync/atomic.a"))
	tg.run("install", "-i", "-pkgdir", pkg, "sync")
	tg.mustExist(filepath.Join(pkg, "sync.a"))
	tg.mustExist(filepath.Join(pkg, "sync/atomic.a"))
}

func TestGoTestRaceInstallCgo(t *testing.T) {
	if !canRace {
		t.Skip("skipping because race detector not supported")
	}

	// golang.org/issue/10500.
	// This used to install a race-enabled cgo.
	tg := testgo(t)
	defer tg.cleanup()
	tg.run("tool", "-n", "cgo")
	cgo := strings.TrimSpace(tg.stdout.String())
	old, err := os.Stat(cgo)
	tg.must(err)

	// For this test, we don't actually care whether 'go test -race -i' succeeds.
	// It may fail, for example, if GOROOT was installed from source as root and
	// is now read-only.
	// We only care that — regardless of whether it succeeds — it does not
	// overwrite cmd/cgo.
	runArgs := []string{"test", "-race", "-i", "runtime/race"}
	if status := tg.doRun(runArgs); status != nil {
		tg.t.Logf("go %v failure ignored: %v", runArgs, status)
	}

	new, err := os.Stat(cgo)
	tg.must(err)
	if !new.ModTime().Equal(old.ModTime()) {
		t.Fatalf("go test -i runtime/race reinstalled cmd/cgo")
	}
}

func TestGoInstallShadowedGOPATH(t *testing.T) {
	// golang.org/issue/3652.
	// go get foo.io (not foo.io/subdir) was not working consistently.

	testenv.MustHaveExternalNetwork(t)

	tg := testgo(t)
	defer tg.cleanup()
	tg.makeTempdir()
	tg.setenv("GOPATH", tg.path("gopath1")+string(filepath.ListSeparator)+tg.path("gopath2"))

	tg.tempDir("gopath1/src/test")
	tg.tempDir("gopath2/src/test")
	tg.tempFile("gopath2/src/test/main.go", "package main\nfunc main(){}\n")

	tg.cd(tg.path("gopath2/src/test"))
	tg.runFail("install")
	tg.grepStderr("no install location for.*gopath2.src.test: hidden by .*gopath1.src.test", "missing error")
}

func TestGoBuildGOPATHOrder(t *testing.T) {
	// golang.org/issue/14176#issuecomment-179895769
	// golang.org/issue/14192
	// -I arguments to compiler could end up not in GOPATH order,
	// leading to unexpected import resolution in the compiler.
	// This is still not a complete fix (see golang.org/issue/14271 and next test)
	// but it is clearly OK and enough to fix both of the two reported
	// instances of the underlying problem. It will have to do for now.

	tg := testgo(t)
	defer tg.cleanup()
	tg.makeTempdir()
	tg.setenv("GOPATH", tg.path("p1")+string(filepath.ListSeparator)+tg.path("p2"))

	tg.tempFile("p1/src/foo/foo.go", "package foo\n")
	tg.tempFile("p2/src/baz/baz.go", "package baz\n")
	tg.tempFile("p2/pkg/"+runtime.GOOS+"_"+runtime.GOARCH+"/foo.a", "bad\n")
	tg.tempFile("p1/src/bar/bar.go", `
		package bar
		import _ "baz"
		import _ "foo"
	`)

	tg.run("install", "-x", "bar")
}

func TestGoBuildGOPATHOrderBroken(t *testing.T) {
	// This test is known not to work.
	// See golang.org/issue/14271.
	t.Skip("golang.org/issue/14271")

	tg := testgo(t)
	defer tg.cleanup()
	tg.makeTempdir()

	tg.tempFile("p1/src/foo/foo.go", "package foo\n")
	tg.tempFile("p2/src/baz/baz.go", "package baz\n")
	tg.tempFile("p1/pkg/"+runtime.GOOS+"_"+runtime.GOARCH+"/baz.a", "bad\n")
	tg.tempFile("p2/pkg/"+runtime.GOOS+"_"+runtime.GOARCH+"/foo.a", "bad\n")
	tg.tempFile("p1/src/bar/bar.go", `
		package bar
		import _ "baz"
		import _ "foo"
	`)

	colon := string(filepath.ListSeparator)
	tg.setenv("GOPATH", tg.path("p1")+colon+tg.path("p2"))
	tg.run("install", "-x", "bar")

	tg.setenv("GOPATH", tg.path("p2")+colon+tg.path("p1"))
	tg.run("install", "-x", "bar")
}

func TestIssue11709(t *testing.T) {
	tg := testgo(t)
	defer tg.cleanup()
	tg.tempFile("run.go", `
		package main
		import "os"
		func main() {
			if os.Getenv("TERM") != "" {
				os.Exit(1)
			}
		}`)
	tg.unsetenv("TERM")
	tg.run("run", tg.path("run.go"))
}

func TestIssue12096(t *testing.T) {
	tg := testgo(t)
	defer tg.cleanup()
	tg.tempFile("test_test.go", `
		package main
		import ("os"; "testing")
		func TestEnv(t *testing.T) {
			if os.Getenv("TERM") != "" {
				t.Fatal("TERM is set")
			}
		}`)
	tg.unsetenv("TERM")
	tg.run("test", tg.path("test_test.go"))
}

func TestGoBuildOutput(t *testing.T) {
	skipIfGccgo(t, "gccgo has no standard packages")
	tooSlow(t)
	tg := testgo(t)
	defer tg.cleanup()

	tg.makeTempdir()
	tg.cd(tg.path("."))

	nonExeSuffix := ".exe"
	if exeSuffix == ".exe" {
		nonExeSuffix = ""
	}

	tg.tempFile("x.go", "package main\nfunc main(){}\n")
	tg.run("build", "x.go")
	tg.wantExecutable("x"+exeSuffix, "go build x.go did not write x"+exeSuffix)
	tg.must(os.Remove(tg.path("x" + exeSuffix)))
	tg.mustNotExist("x" + nonExeSuffix)

	tg.run("build", "-o", "myprog", "x.go")
	tg.mustNotExist("x")
	tg.mustNotExist("x.exe")
	tg.wantExecutable("myprog", "go build -o myprog x.go did not write myprog")
	tg.mustNotExist("myprog.exe")

	tg.tempFile("p.go", "package p\n")
	tg.run("build", "p.go")
	tg.mustNotExist("p")
	tg.mustNotExist("p.a")
	tg.mustNotExist("p.o")
	tg.mustNotExist("p.exe")

	tg.run("build", "-o", "p.a", "p.go")
	tg.wantArchive("p.a")

	tg.run("build", "cmd/gofmt")
	tg.wantExecutable("gofmt"+exeSuffix, "go build cmd/gofmt did not write gofmt"+exeSuffix)
	tg.must(os.Remove(tg.path("gofmt" + exeSuffix)))
	tg.mustNotExist("gofmt" + nonExeSuffix)

	tg.run("build", "-o", "mygofmt", "cmd/gofmt")
	tg.wantExecutable("mygofmt", "go build -o mygofmt cmd/gofmt did not write mygofmt")
	tg.mustNotExist("mygofmt.exe")
	tg.mustNotExist("gofmt")
	tg.mustNotExist("gofmt.exe")

	tg.run("build", "sync/atomic")
	tg.mustNotExist("atomic")
	tg.mustNotExist("atomic.exe")

	tg.run("build", "-o", "myatomic.a", "sync/atomic")
	tg.wantArchive("myatomic.a")
	tg.mustNotExist("atomic")
	tg.mustNotExist("atomic.a")
	tg.mustNotExist("atomic.exe")

	tg.runFail("build", "-o", "whatever", "cmd/gofmt", "sync/atomic")
	tg.grepStderr("multiple packages", "did not reject -o with multiple packages")
}

func TestGoBuildARM(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping cross-compile in short mode")
	}

	tg := testgo(t)
	defer tg.cleanup()

	tg.makeTempdir()
	tg.cd(tg.path("."))

	tg.setenv("GOARCH", "arm")
	tg.setenv("GOOS", "linux")
	tg.setenv("GOARM", "5")
	tg.tempFile("hello.go", `package main
		func main() {}`)
	tg.run("build", "hello.go")
	tg.grepStderrNot("unable to find math.a", "did not build math.a correctly")
}

// For issue 14337.
func TestParallelTest(t *testing.T) {
	tooSlow(t)
	tg := testgo(t)
	tg.parallel()
	defer tg.cleanup()
	tg.makeTempdir()
	const testSrc = `package package_test
		import (
			"testing"
		)
		func TestTest(t *testing.T) {
		}`
	tg.tempFile("src/p1/p1_test.go", strings.Replace(testSrc, "package_test", "p1_test", 1))
	tg.tempFile("src/p2/p2_test.go", strings.Replace(testSrc, "package_test", "p2_test", 1))
	tg.tempFile("src/p3/p3_test.go", strings.Replace(testSrc, "package_test", "p3_test", 1))
	tg.tempFile("src/p4/p4_test.go", strings.Replace(testSrc, "package_test", "p4_test", 1))
	tg.setenv("GOPATH", tg.path("."))
	tg.run("test", "-p=4", "p1", "p2", "p3", "p4")
}

func TestBinaryOnlyPackages(t *testing.T) {
	tooSlow(t)

	tg := testgo(t)
	defer tg.cleanup()
	tg.parallel()
	tg.makeTempdir()
	tg.setenv("GOPATH", tg.path("."))

	tg.tempFile("src/p1/p1.go", `//go:binary-only-package

		package p1
	`)
	tg.wantStale("p1", "binary-only packages are no longer supported", "p1 is binary-only, and this message should always be printed")
	tg.runFail("install", "p1")
	tg.grepStderr("binary-only packages are no longer supported", "did not report attempt to compile binary-only package")

	tg.tempFile("src/p1/p1.go", `
		package p1
		import "fmt"
		func F(b bool) { fmt.Printf("hello from p1\n"); if b { F(false) } }
	`)
	tg.run("install", "p1")
	os.Remove(tg.path("src/p1/p1.go"))
	tg.mustNotExist(tg.path("src/p1/p1.go"))

	tg.tempFile("src/p2/p2.go", `//go:binary-only-packages-are-not-great

		package p2
		import "p1"
		func F() { p1.F(true) }
	`)
	tg.runFail("install", "p2")
	tg.grepStderr("no Go files", "did not complain about missing sources")

	tg.tempFile("src/p1/missing.go", `//go:binary-only-package

		package p1
		import _ "fmt"
		func G()
	`)
	tg.wantStale("p1", "binary-only package", "should NOT want to rebuild p1 (first)")
	tg.runFail("install", "p2")
	tg.grepStderr("p1: binary-only packages are no longer supported", "did not report error for binary-only p1")

	tg.run("list", "-deps", "-f", "{{.ImportPath}}: {{.BinaryOnly}}", "p2")
	tg.grepStdout("p1: true", "p1 not listed as BinaryOnly")
	tg.grepStdout("p2: false", "p2 listed as BinaryOnly")
}

// Issue 16050.
func TestAlwaysLinkSysoFiles(t *testing.T) {
	tg := testgo(t)
	defer tg.cleanup()
	tg.parallel()
	tg.tempDir("src/syso")
	tg.tempFile("src/syso/a.syso", ``)
	tg.tempFile("src/syso/b.go", `package syso`)
	tg.setenv("GOPATH", tg.path("."))

	// We should see the .syso file regardless of the setting of
	// CGO_ENABLED.

	tg.setenv("CGO_ENABLED", "1")
	tg.run("list", "-f", "{{.SysoFiles}}", "syso")
	tg.grepStdout("a.syso", "missing syso file with CGO_ENABLED=1")

	tg.setenv("CGO_ENABLED", "0")
	tg.run("list", "-f", "{{.SysoFiles}}", "syso")
	tg.grepStdout("a.syso", "missing syso file with CGO_ENABLED=0")
}

// Issue 16120.
func TestGenerateUsesBuildContext(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("this test won't run under Windows")
	}

	tg := testgo(t)
	defer tg.cleanup()
	tg.parallel()
	tg.tempDir("src/gen")
	tg.tempFile("src/gen/gen.go", "package gen\n//go:generate echo $GOOS $GOARCH\n")
	tg.setenv("GOPATH", tg.path("."))

	tg.setenv("GOOS", "linux")
	tg.setenv("GOARCH", "amd64")
	tg.run("generate", "gen")
	tg.grepStdout("linux amd64", "unexpected GOOS/GOARCH combination")

	tg.setenv("GOOS", "darwin")
	tg.setenv("GOARCH", "386")
	tg.run("generate", "gen")
	tg.grepStdout("darwin 386", "unexpected GOOS/GOARCH combination")
}

func TestGoEnv(t *testing.T) {
	tg := testgo(t)
	tg.parallel()
	defer tg.cleanup()
	tg.setenv("GOOS", "freebsd") // to avoid invalid pair errors
	tg.setenv("GOARCH", "arm")
	tg.run("env", "GOARCH")
	tg.grepStdout("^arm$", "GOARCH not honored")

	tg.run("env", "GCCGO")
	tg.grepStdout(".", "GCCGO unexpectedly empty")

	tg.run("env", "CGO_CFLAGS")
	tg.grepStdout(".", "default CGO_CFLAGS unexpectedly empty")

	tg.setenv("CGO_CFLAGS", "-foobar")
	tg.run("env", "CGO_CFLAGS")
	tg.grepStdout("^-foobar$", "CGO_CFLAGS not honored")

	tg.setenv("CC", "gcc -fmust -fgo -ffaster")
	tg.run("env", "CC")
	tg.grepStdout("gcc", "CC not found")
	tg.run("env", "GOGCCFLAGS")
	tg.grepStdout("-ffaster", "CC arguments not found")
}

const (
	noMatchesPattern = `(?m)^ok.*\[no tests to run\]`
	okPattern        = `(?m)^ok`
)

// Issue 19394
func TestWriteProfilesOnTimeout(t *testing.T) {
	tooSlow(t)
	tg := testgo(t)
	defer tg.cleanup()
	tg.tempDir("profiling")
	tg.tempFile("profiling/timeouttest_test.go", `package timeouttest_test
import "testing"
import "time"
func TestSleep(t *testing.T) { time.Sleep(time.Second) }`)
	tg.cd(tg.path("profiling"))
	tg.runFail(
		"test",
		"-cpuprofile", tg.path("profiling/cpu.pprof"), "-memprofile", tg.path("profiling/mem.pprof"),
		"-timeout", "1ms")
	tg.mustHaveContent(tg.path("profiling/cpu.pprof"))
	tg.mustHaveContent(tg.path("profiling/mem.pprof"))
}

func TestLinkXImportPathEscape(t *testing.T) {
	// golang.org/issue/16710
	skipIfGccgo(t, "gccgo does not support -ldflags -X")
	tg := testgo(t)
	defer tg.cleanup()
	tg.parallel()
	tg.makeTempdir()
	tg.setenv("GOPATH", filepath.Join(tg.pwd(), "testdata"))
	exe := tg.path("linkx" + exeSuffix)
	tg.creatingTemp(exe)
	tg.run("build", "-o", exe, "-ldflags", "-X=my.pkg.Text=linkXworked", "my.pkg/main")
	out, err := exec.Command(exe).CombinedOutput()
	if err != nil {
		tg.t.Fatal(err)
	}
	if string(out) != "linkXworked\n" {
		tg.t.Log(string(out))
		tg.t.Fatal(`incorrect output: expected "linkXworked\n"`)
	}
}

// Issue 18044.
func TestLdBindNow(t *testing.T) {
	tg := testgo(t)
	defer tg.cleanup()
	tg.parallel()
	tg.setenv("LD_BIND_NOW", "1")
	tg.run("help")
}

// Issue 18225.
// This is really a cmd/asm issue but this is a convenient place to test it.
func TestConcurrentAsm(t *testing.T) {
	skipIfGccgo(t, "gccgo does not use cmd/asm")
	tg := testgo(t)
	defer tg.cleanup()
	tg.parallel()
	asm := `DATA ·constants<>+0x0(SB)/8,$0
GLOBL ·constants<>(SB),8,$8
`
	tg.tempFile("go/src/p/a.s", asm)
	tg.tempFile("go/src/p/b.s", asm)
	tg.tempFile("go/src/p/p.go", `package p`)
	tg.setenv("GOPATH", tg.path("go"))
	tg.run("build", "p")
}

// Issue 18778.
func TestDotDotDotOutsideGOPATH(t *testing.T) {
	tg := testgo(t)
	defer tg.cleanup()

	tg.tempFile("pkgs/a.go", `package x`)
	tg.tempFile("pkgs/a_test.go", `package x_test
import "testing"
func TestX(t *testing.T) {}`)

	tg.tempFile("pkgs/a/a.go", `package a`)
	tg.tempFile("pkgs/a/a_test.go", `package a_test
import "testing"
func TestA(t *testing.T) {}`)

	tg.cd(tg.path("pkgs"))
	tg.run("build", "./...")
	tg.run("test", "./...")
	tg.run("list", "./...")
	tg.grepStdout("pkgs$", "expected package not listed")
	tg.grepStdout("pkgs/a", "expected package not listed")
}

// Issue 18975.
func TestFFLAGS(t *testing.T) {
	if !canCgo {
		t.Skip("skipping because cgo not enabled")
	}

	tg := testgo(t)
	defer tg.cleanup()
	tg.parallel()

	tg.tempFile("p/src/p/main.go", `package main
		// #cgo FFLAGS: -no-such-fortran-flag
		import "C"
		func main() {}
	`)
	tg.tempFile("p/src/p/a.f", `! comment`)
	tg.setenv("GOPATH", tg.path("p"))

	// This should normally fail because we are passing an unknown flag,
	// but issue #19080 points to Fortran compilers that succeed anyhow.
	// To work either way we call doRun directly rather than run or runFail.
	tg.doRun([]string{"build", "-x", "p"})

	tg.grepStderr("no-such-fortran-flag", `missing expected "-no-such-fortran-flag"`)
}

// Issue 19198.
// This is really a cmd/link issue but this is a convenient place to test it.
func TestDuplicateGlobalAsmSymbols(t *testing.T) {
	skipIfGccgo(t, "gccgo does not use cmd/asm")
	tooSlow(t)
	if runtime.GOARCH != "386" && runtime.GOARCH != "amd64" {
		t.Skipf("skipping test on %s", runtime.GOARCH)
	}
	if !canCgo {
		t.Skip("skipping because cgo not enabled")
	}

	tg := testgo(t)
	defer tg.cleanup()
	tg.parallel()

	asm := `
#include "textflag.h"

DATA sym<>+0x0(SB)/8,$0
GLOBL sym<>(SB),(NOPTR+RODATA),$8

TEXT ·Data(SB),NOSPLIT,$0
	MOVB sym<>(SB), AX
	MOVB AX, ret+0(FP)
	RET
`
	tg.tempFile("go/src/a/a.s", asm)
	tg.tempFile("go/src/a/a.go", `package a; func Data() uint8`)
	tg.tempFile("go/src/b/b.s", asm)
	tg.tempFile("go/src/b/b.go", `package b; func Data() uint8`)
	tg.tempFile("go/src/p/p.go", `
package main
import "a"
import "b"
import "C"
func main() {
	_ = a.Data() + b.Data()
}
`)
	tg.setenv("GOPATH", tg.path("go"))
	exe := tg.path("p.exe")
	tg.creatingTemp(exe)
	tg.run("build", "-o", exe, "p")
}

func TestBuildTagsNoComma(t *testing.T) {
	skipIfGccgo(t, "gccgo has no standard packages")
	tg := testgo(t)
	defer tg.cleanup()
	tg.makeTempdir()
	tg.setenv("GOPATH", tg.path("go"))
	tg.run("build", "-tags", "tag1 tag2", "math")
	tg.runFail("build", "-tags", "tag1,tag2 tag3", "math")
	tg.grepBoth("space-separated list contains comma", "-tags with a comma-separated list didn't error")
}

func copyFile(src, dst string, perm os.FileMode) error {
	sf, err := os.Open(src)
	if err != nil {
		return err
	}
	defer sf.Close()

	df, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}

	_, err = io.Copy(df, sf)
	err2 := df.Close()
	if err != nil {
		return err
	}
	return err2
}

// TestExecutableGOROOT verifies that the cmd/go binary itself uses
// os.Executable (when available) to locate GOROOT.
func TestExecutableGOROOT(t *testing.T) {
	skipIfGccgo(t, "gccgo has no GOROOT")

	// Note: Must not call tg methods inside subtests: tg is attached to outer t.
	tg := testgo(t)
	tg.unsetenv("GOROOT")
	defer tg.cleanup()

	check := func(t *testing.T, exe, want string) {
		cmd := exec.Command(exe, "env", "GOROOT")
		cmd.Env = tg.env
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s env GOROOT: %v, %s", exe, err, out)
		}
		goroot, err := filepath.EvalSymlinks(strings.TrimSpace(string(out)))
		if err != nil {
			t.Fatal(err)
		}
		want, err = filepath.EvalSymlinks(want)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.EqualFold(goroot, want) {
			t.Errorf("go env GOROOT:\nhave %s\nwant %s", goroot, want)
		} else {
			t.Logf("go env GOROOT: %s", goroot)
		}
	}

	tg.makeTempdir()
	tg.tempDir("new/bin")
	newGoTool := tg.path("new/bin/go" + exeSuffix)
	tg.must(copyFile(tg.goTool(), newGoTool, 0775))
	newRoot := tg.path("new")

	t.Run("RelocatedExe", func(t *testing.T) {
		// Should fall back to default location in binary,
		// which is the GOROOT we used when building testgo.exe.
		check(t, newGoTool, testGOROOT)
	})

	// If the binary is sitting in a bin dir next to ../pkg/tool, that counts as a GOROOT,
	// so it should find the new tree.
	tg.tempDir("new/pkg/tool")
	t.Run("RelocatedTree", func(t *testing.T) {
		check(t, newGoTool, newRoot)
	})

	tg.tempDir("other/bin")
	symGoTool := tg.path("other/bin/go" + exeSuffix)

	// Symlink into go tree should still find go tree.
	t.Run("SymlinkedExe", func(t *testing.T) {
		testenv.MustHaveSymlink(t)
		if err := os.Symlink(newGoTool, symGoTool); err != nil {
			t.Fatal(err)
		}
		check(t, symGoTool, newRoot)
	})

	tg.must(robustio.RemoveAll(tg.path("new/pkg")))

	// Binaries built in the new tree should report the
	// new tree when they call runtime.GOROOT.
	t.Run("RuntimeGoroot", func(t *testing.T) {
		// Build a working GOROOT the easy way, with symlinks.
		testenv.MustHaveSymlink(t)
		if err := os.Symlink(filepath.Join(testGOROOT, "src"), tg.path("new/src")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(testGOROOT, "pkg"), tg.path("new/pkg")); err != nil {
			t.Fatal(err)
		}

		cmd := exec.Command(newGoTool, "run", "testdata/print_goroot.go")
		cmd.Env = tg.env
		cmd.Stderr = os.Stderr
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("%s run testdata/print_goroot.go: %v, %s", newGoTool, err, out)
		}
		goroot, err := filepath.EvalSymlinks(strings.TrimSpace(string(out)))
		if err != nil {
			t.Fatal(err)
		}
		want, err := filepath.EvalSymlinks(tg.path("new"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.EqualFold(goroot, want) {
			t.Errorf("go run testdata/print_goroot.go:\nhave %s\nwant %s", goroot, want)
		} else {
			t.Logf("go run testdata/print_goroot.go: %s", goroot)
		}
	})
}

func TestNeedVersion(t *testing.T) {
	skipIfGccgo(t, "gccgo does not use cmd/compile")
	tg := testgo(t)
	defer tg.cleanup()
	tg.parallel()
	tg.tempFile("goversion.go", `package main; func main() {}`)
	path := tg.path("goversion.go")
	tg.setenv("TESTGO_VERSION", "go1.testgo")
	tg.runFail("run", path)
	tg.grepStderr("compile", "does not match go tool version")
}

func TestCgoFlagContainsSpace(t *testing.T) {
	tooSlow(t)
	if !canCgo {
		t.Skip("skipping because cgo not enabled")
	}
	tg := testgo(t)
	defer tg.cleanup()

	tg.makeTempdir()
	tg.cd(tg.path("."))
	tg.tempFile("main.go", `package main
		// #cgo CFLAGS: -I"c flags"
		// #cgo LDFLAGS: -L"ld flags"
		import "C"
		func main() {}
	`)
	tg.run("run", "-x", "main.go")
	tg.grepStderr(`"-I[^"]+c flags"`, "did not find quoted c flags")
	tg.grepStderrNot(`"-I[^"]+c flags".*"-I[^"]+c flags"`, "found too many quoted c flags")
	tg.grepStderr(`"-L[^"]+ld flags"`, "did not find quoted ld flags")
	tg.grepStderrNot(`"-L[^"]+c flags".*"-L[^"]+c flags"`, "found too many quoted ld flags")
}

// Issue 9737: verify that GOARM and GO386 affect the computed build ID.
func TestBuildIDContainsArchModeEnv(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}

	var tg *testgoData
	testWith := func(before, after func()) func(*testing.T) {
		return func(t *testing.T) {
			tg = testgo(t)
			defer tg.cleanup()
			tg.tempFile("src/mycmd/x.go", `package main
func main() {}`)
			tg.setenv("GOPATH", tg.path("."))

			tg.cd(tg.path("src/mycmd"))
			tg.setenv("GOOS", "linux")
			before()
			tg.run("install", "mycmd")
			after()
			tg.wantStale("mycmd", "stale dependency", "should be stale after environment variable change")
		}
	}

	t.Run("386", testWith(func() {
		tg.setenv("GOARCH", "386")
		tg.setenv("GO386", "387")
	}, func() {
		tg.setenv("GO386", "sse2")
	}))

	t.Run("arm", testWith(func() {
		tg.setenv("GOARCH", "arm")
		tg.setenv("GOARM", "5")
	}, func() {
		tg.setenv("GOARM", "7")
	}))
}

func TestBuildmodePIE(t *testing.T) {
	if testing.Short() && testenv.Builder() == "" {
		t.Skipf("skipping in -short mode on non-builder")
	}

	platform := fmt.Sprintf("%s/%s", runtime.GOOS, runtime.GOARCH)
	switch platform {
	case "linux/386", "linux/amd64", "linux/arm", "linux/arm64", "linux/ppc64le", "linux/s390x",
		"android/amd64", "android/arm", "android/arm64", "android/386",
		"freebsd/amd64":
	case "darwin/amd64":
	default:
		t.Skipf("skipping test because buildmode=pie is not supported on %s", platform)
	}

	tg := testgo(t)
	defer tg.cleanup()
	tg.parallel()

	tg.tempFile("main.go", `package main; func main() { print("hello") }`)
	src := tg.path("main.go")
	obj := tg.path("main")
	tg.run("build", "-buildmode=pie", "-o", obj, src)

	switch runtime.GOOS {
	case "linux", "android", "freebsd":
		f, err := elf.Open(obj)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if f.Type != elf.ET_DYN {
			t.Errorf("PIE type must be ET_DYN, but %s", f.Type)
		}
	case "darwin":
		f, err := macho.Open(obj)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if f.Flags&macho.FlagDyldLink == 0 {
			t.Error("PIE must have DyldLink flag, but not")
		}
		if f.Flags&macho.FlagPIE == 0 {
			t.Error("PIE must have PIE flag, but not")
		}
	default:
		panic("unreachable")
	}

	out, err := exec.Command(obj).CombinedOutput()
	if err != nil {
		t.Fatal(err)
	}

	if string(out) != "hello" {
		t.Errorf("got %q; want %q", out, "hello")
	}
}

func TestExecBuildX(t *testing.T) {
	tooSlow(t)
	if !canCgo {
		t.Skip("skipping because cgo not enabled")
	}

	testenv.MustHaveExecPath(t, "/usr/bin/env")
	testenv.MustHaveExecPath(t, "bash")

	tg := testgo(t)
	defer tg.cleanup()

	tg.tempDir("cache")
	tg.setenv("GOCACHE", tg.path("cache"))

	// Before building our test main.go, ensure that an up-to-date copy of
	// runtime/cgo is present in the cache. If it isn't, the 'go build' step below
	// will fail with "can't open import". See golang.org/issue/29004.
	tg.run("build", "runtime/cgo")

	tg.tempFile("main.go", `package main; import "C"; func main() { print("hello") }`)
	src := tg.path("main.go")
	obj := tg.path("main")
	tg.run("build", "-x", "-o", obj, src)
	sh := tg.path("test.sh")
	cmds := tg.getStderr()
	err := ioutil.WriteFile(sh, []byte("set -e\n"+cmds), 0666)
	if err != nil {
		t.Fatal(err)
	}

	out, err := exec.Command(obj).CombinedOutput()
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "hello" {
		t.Fatalf("got %q; want %q", out, "hello")
	}

	err = os.Remove(obj)
	if err != nil {
		t.Fatal(err)
	}

	out, err = exec.Command("/usr/bin/env", "bash", "-x", sh).CombinedOutput()
	if err != nil {
		t.Fatalf("/bin/sh %s: %v\n%s", sh, err, out)
	}
	t.Logf("shell output:\n%s", out)

	out, err = exec.Command(obj).CombinedOutput()
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "hello" {
		t.Fatalf("got %q; want %q", out, "hello")
	}

	matches := regexp.MustCompile(`^WORK=(.*)\n`).FindStringSubmatch(cmds)
	if len(matches) == 0 {
		t.Fatal("no WORK directory")
	}
	tg.must(robustio.RemoveAll(matches[1]))
}

func TestUpxCompression(t *testing.T) {
	if runtime.GOOS != "linux" ||
		(runtime.GOARCH != "amd64" && runtime.GOARCH != "386") {
		t.Skipf("skipping upx test on %s/%s", runtime.GOOS, runtime.GOARCH)
	}

	testenv.MustHaveExecPath(t, "upx")
	out, err := exec.Command("upx", "--version").CombinedOutput()
	if err != nil {
		t.Fatalf("upx --version failed: %v", err)
	}

	// upx --version prints `upx <version>` in the first line of output:
	//   upx 3.94
	//   [...]
	re := regexp.MustCompile(`([[:digit:]]+)\.([[:digit:]]+)`)
	upxVersion := re.FindStringSubmatch(string(out))
	if len(upxVersion) != 3 {
		t.Fatalf("bad upx version string: %s", upxVersion)
	}

	major, err1 := strconv.Atoi(upxVersion[1])
	minor, err2 := strconv.Atoi(upxVersion[2])
	if err1 != nil || err2 != nil {
		t.Fatalf("bad upx version string: %s", upxVersion[0])
	}

	// Anything below 3.94 is known not to work with go binaries
	if (major < 3) || (major == 3 && minor < 94) {
		t.Skipf("skipping because upx version %v.%v is too old", major, minor)
	}

	tg := testgo(t)
	defer tg.cleanup()
	tg.parallel()

	tg.tempFile("main.go", `package main; import "fmt"; func main() { fmt.Print("hello upx") }`)
	src := tg.path("main.go")
	obj := tg.path("main")
	tg.run("build", "-o", obj, src)

	out, err = exec.Command("upx", obj).CombinedOutput()
	if err != nil {
		t.Logf("executing upx\n%s\n", out)
		t.Fatalf("upx failed with %v", err)
	}

	out, err = exec.Command(obj).CombinedOutput()
	if err != nil {
		t.Logf("%s", out)
		t.Fatalf("running compressed go binary failed with error %s", err)
	}
	if string(out) != "hello upx" {
		t.Fatalf("bad output from compressed go binary:\ngot %q; want %q", out, "hello upx")
	}
}

func TestCacheListStale(t *testing.T) {
	tooSlow(t)
	if strings.Contains(os.Getenv("GODEBUG"), "gocacheverify") {
		t.Skip("GODEBUG gocacheverify")
	}
	tg := testgo(t)
	defer tg.cleanup()
	tg.parallel()
	tg.makeTempdir()
	tg.setenv("GOCACHE", tg.path("cache"))
	tg.tempFile("gopath/src/p/p.go", "package p; import _ \"q\"; func F(){}\n")
	tg.tempFile("gopath/src/q/q.go", "package q; func F(){}\n")
	tg.tempFile("gopath/src/m/m.go", "package main; import _ \"q\"; func main(){}\n")

	tg.setenv("GOPATH", tg.path("gopath"))
	tg.run("install", "p", "m")
	tg.run("list", "-f={{.ImportPath}} {{.Stale}}", "m", "q", "p")
	tg.grepStdout("^m false", "m should not be stale")
	tg.grepStdout("^q true", "q should be stale")
	tg.grepStdout("^p false", "p should not be stale")
}

func TestCacheCoverage(t *testing.T) {
	tooSlow(t)

	if strings.Contains(os.Getenv("GODEBUG"), "gocacheverify") {
		t.Skip("GODEBUG gocacheverify")
	}

	tg := testgo(t)
	defer tg.cleanup()
	tg.parallel()
	tg.setenv("GOPATH", filepath.Join(tg.pwd(), "testdata"))
	tg.makeTempdir()

	tg.setenv("GOCACHE", tg.path("c1"))
	tg.run("test", "-cover", "-short", "strings")
	tg.run("test", "-cover", "-short", "math", "strings")
}

func TestIssue22588(t *testing.T) {
	// Don't get confused by stderr coming from tools.
	tg := testgo(t)
	defer tg.cleanup()
	tg.parallel()

	if _, err := os.Stat("/usr/bin/time"); err != nil {
		t.Skip(err)
	}

	tg.run("list", "-f={{.Stale}}", "runtime")
	tg.run("list", "-toolexec=/usr/bin/time", "-f={{.Stale}}", "runtime")
	tg.grepStdout("false", "incorrectly reported runtime as stale")
}

func TestIssue22531(t *testing.T) {
	tooSlow(t)
	if strings.Contains(os.Getenv("GODEBUG"), "gocacheverify") {
		t.Skip("GODEBUG gocacheverify")
	}
	tg := testgo(t)
	defer tg.cleanup()
	tg.parallel()
	tg.makeTempdir()
	tg.setenv("GOPATH", tg.tempdir)
	tg.setenv("GOCACHE", tg.path("cache"))
	tg.tempFile("src/m/main.go", "package main /* c1 */; func main() {}\n")
	tg.run("install", "-x", "m")
	tg.run("list", "-f", "{{.Stale}}", "m")
	tg.grepStdout("false", "reported m as stale after install")
	tg.run("tool", "buildid", tg.path("bin/m"+exeSuffix))

	// The link action ID did not include the full main build ID,
	// even though the full main build ID is written into the
	// eventual binary. That caused the following install to
	// be a no-op, thinking the gofmt binary was up-to-date,
	// even though .Stale could see it was not.
	tg.tempFile("src/m/main.go", "package main /* c2 */; func main() {}\n")
	tg.run("install", "-x", "m")
	tg.run("list", "-f", "{{.Stale}}", "m")
	tg.grepStdout("false", "reported m as stale after reinstall")
	tg.run("tool", "buildid", tg.path("bin/m"+exeSuffix))
}

func TestIssue22596(t *testing.T) {
	tooSlow(t)
	if strings.Contains(os.Getenv("GODEBUG"), "gocacheverify") {
		t.Skip("GODEBUG gocacheverify")
	}
	tg := testgo(t)
	defer tg.cleanup()
	tg.parallel()
	tg.makeTempdir()
	tg.setenv("GOCACHE", tg.path("cache"))
	tg.tempFile("gopath1/src/p/p.go", "package p; func F(){}\n")
	tg.tempFile("gopath2/src/p/p.go", "package p; func F(){}\n")

	tg.setenv("GOPATH", tg.path("gopath1"))
	tg.run("list", "-f={{.Target}}", "p")
	target1 := strings.TrimSpace(tg.getStdout())
	tg.run("install", "p")
	tg.wantNotStale("p", "", "p stale after install")

	tg.setenv("GOPATH", tg.path("gopath2"))
	tg.run("list", "-f={{.Target}}", "p")
	target2 := strings.TrimSpace(tg.getStdout())
	tg.must(os.MkdirAll(filepath.Dir(target2), 0777))
	tg.must(copyFile(target1, target2, 0666))
	tg.wantStale("p", "build ID mismatch", "p not stale after copy from gopath1")
	tg.run("install", "p")
	tg.wantNotStale("p", "", "p stale after install2")
}

func TestTestCache(t *testing.T) {
	tooSlow(t)

	if strings.Contains(os.Getenv("GODEBUG"), "gocacheverify") {
		t.Skip("GODEBUG gocacheverify")
	}
	tg := testgo(t)
	defer tg.cleanup()
	tg.parallel()
	tg.makeTempdir()
	tg.setenv("GOPATH", tg.tempdir)
	tg.setenv("GOCACHE", tg.path("cache"))

	if runtime.Compiler != "gccgo" {
		// timeout here should not affect result being cached
		// or being retrieved later.
		tg.run("test", "-x", "-timeout=10s", "errors")
		tg.grepStderr(`[\\/]compile|gccgo`, "did not run compiler")
		tg.grepStderr(`[\\/]link|gccgo`, "did not run linker")
		tg.grepStderr(`errors\.test`, "did not run test")

		tg.run("test", "-x", "errors")
		tg.grepStdout(`ok  \terrors\t\(cached\)`, "did not report cached result")
		tg.grepStderrNot(`[\\/]compile|gccgo`, "incorrectly ran compiler")
		tg.grepStderrNot(`[\\/]link|gccgo`, "incorrectly ran linker")
		tg.grepStderrNot(`errors\.test`, "incorrectly ran test")
		tg.grepStderrNot("DO NOT USE", "poisoned action status leaked")

		// Even very low timeouts do not disqualify cached entries.
		tg.run("test", "-timeout=1ns", "-x", "errors")
		tg.grepStderrNot(`errors\.test`, "incorrectly ran test")

		tg.run("clean", "-testcache")
		tg.run("test", "-x", "errors")
		tg.grepStderr(`errors\.test`, "did not run test")
	}

	// The -p=1 in the commands below just makes the -x output easier to read.

	t.Log("\n\nINITIAL\n\n")

	tg.tempFile("src/p1/p1.go", "package p1\nvar X =  1\n")
	tg.tempFile("src/p2/p2.go", "package p2\nimport _ \"p1\"\nvar X = 1\n")
	tg.tempFile("src/t/t1/t1_test.go", "package t\nimport \"testing\"\nfunc Test1(*testing.T) {}\n")
	tg.tempFile("src/t/t2/t2_test.go", "package t\nimport _ \"p1\"\nimport \"testing\"\nfunc Test2(*testing.T) {}\n")
	tg.tempFile("src/t/t3/t3_test.go", "package t\nimport \"p1\"\nimport \"testing\"\nfunc Test3(t *testing.T) {t.Log(p1.X)}\n")
	tg.tempFile("src/t/t4/t4_test.go", "package t\nimport \"p2\"\nimport \"testing\"\nfunc Test4(t *testing.T) {t.Log(p2.X)}")
	tg.run("test", "-x", "-v", "-short", "t/...")

	t.Log("\n\nREPEAT\n\n")

	tg.run("test", "-x", "-v", "-short", "t/...")
	tg.grepStdout(`ok  \tt/t1\t\(cached\)`, "did not cache t1")
	tg.grepStdout(`ok  \tt/t2\t\(cached\)`, "did not cache t2")
	tg.grepStdout(`ok  \tt/t3\t\(cached\)`, "did not cache t3")
	tg.grepStdout(`ok  \tt/t4\t\(cached\)`, "did not cache t4")
	tg.grepStderrNot(`[\\/](compile|gccgo) `, "incorrectly ran compiler")
	tg.grepStderrNot(`[\\/](link|gccgo) `, "incorrectly ran linker")
	tg.grepStderrNot(`p[0-9]\.test`, "incorrectly ran test")

	t.Log("\n\nCOMMENT\n\n")

	// Changing the program text without affecting the compiled package
	// should result in the package being rebuilt but nothing more.
	tg.tempFile("src/p1/p1.go", "package p1\nvar X = 01\n")
	tg.run("test", "-p=1", "-x", "-v", "-short", "t/...")
	tg.grepStdout(`ok  \tt/t1\t\(cached\)`, "did not cache t1")
	tg.grepStdout(`ok  \tt/t2\t\(cached\)`, "did not cache t2")
	tg.grepStdout(`ok  \tt/t3\t\(cached\)`, "did not cache t3")
	tg.grepStdout(`ok  \tt/t4\t\(cached\)`, "did not cache t4")
	tg.grepStderrNot(`([\\/](compile|gccgo) ).*t[0-9]_test\.go`, "incorrectly ran compiler")
	tg.grepStderrNot(`[\\/](link|gccgo) `, "incorrectly ran linker")
	tg.grepStderrNot(`t[0-9]\.test.*test\.short`, "incorrectly ran test")

	t.Log("\n\nCHANGE\n\n")

	// Changing the actual package should have limited effects.
	tg.tempFile("src/p1/p1.go", "package p1\nvar X = 02\n")
	tg.run("test", "-p=1", "-x", "-v", "-short", "t/...")

	// p2 should have been rebuilt.
	tg.grepStderr(`([\\/]compile|gccgo).*p2.go`, "did not recompile p2")

	// t1 does not import anything, should not have been rebuilt.
	tg.grepStderrNot(`([\\/]compile|gccgo).*t1_test.go`, "incorrectly recompiled t1")
	tg.grepStderrNot(`([\\/]link|gccgo).*t1_test`, "incorrectly relinked t1_test")
	tg.grepStdout(`ok  \tt/t1\t\(cached\)`, "did not cache t/t1")

	// t2 imports p1 and must be rebuilt and relinked,
	// but the change should not have any effect on the test binary,
	// so the test should not have been rerun.
	tg.grepStderr(`([\\/]compile|gccgo).*t2_test.go`, "did not recompile t2")
	tg.grepStderr(`([\\/]link|gccgo).*t2\.test`, "did not relink t2_test")
	// This check does not currently work with gccgo, as garbage
	// collection of unused variables is not turned on by default.
	if runtime.Compiler != "gccgo" {
		tg.grepStdout(`ok  \tt/t2\t\(cached\)`, "did not cache t/t2")
	}

	// t3 imports p1, and changing X changes t3's test binary.
	tg.grepStderr(`([\\/]compile|gccgo).*t3_test.go`, "did not recompile t3")
	tg.grepStderr(`([\\/]link|gccgo).*t3\.test`, "did not relink t3_test")
	tg.grepStderr(`t3\.test.*-test.short`, "did not rerun t3_test")
	tg.grepStdoutNot(`ok  \tt/t3\t\(cached\)`, "reported cached t3_test result")

	// t4 imports p2, but p2 did not change, so t4 should be relinked, not recompiled,
	// and not rerun.
	tg.grepStderrNot(`([\\/]compile|gccgo).*t4_test.go`, "incorrectly recompiled t4")
	tg.grepStderr(`([\\/]link|gccgo).*t4\.test`, "did not relink t4_test")
	// This check does not currently work with gccgo, as garbage
	// collection of unused variables is not turned on by default.
	if runtime.Compiler != "gccgo" {
		tg.grepStdout(`ok  \tt/t4\t\(cached\)`, "did not cache t/t4")
	}
}

func TestTestSkipVetAfterFailedBuild(t *testing.T) {
	tg := testgo(t)
	defer tg.cleanup()
	tg.parallel()

	tg.tempFile("x_test.go", `package x
		func f() {
			return 1
		}
	`)

	tg.runFail("test", tg.path("x_test.go"))
	tg.grepStderrNot(`vet`, "vet should be skipped after the failed build")
}

func TestTestVetRebuild(t *testing.T) {
	tooSlow(t)
	tg := testgo(t)
	defer tg.cleanup()
	tg.parallel()

	// golang.org/issue/23701.
	// b_test imports b with augmented method from export_test.go.
	// b_test also imports a, which imports b.
	// Must not accidentally see un-augmented b propagate through a to b_test.
	tg.tempFile("src/a/a.go", `package a
		import "b"
		type Type struct{}
		func (*Type) M() b.T {return 0}
	`)
	tg.tempFile("src/b/b.go", `package b
		type T int
		type I interface {M() T}
	`)
	tg.tempFile("src/b/export_test.go", `package b
		func (*T) Method() *T { return nil }
	`)
	tg.tempFile("src/b/b_test.go", `package b_test
		import (
			"testing"
			"a"
			. "b"
		)
		func TestBroken(t *testing.T) {
			x := new(T)
			x.Method()
			_ = new(a.Type)
		}
	`)

	tg.setenv("GOPATH", tg.path("."))
	tg.run("test", "b")
	tg.run("vet", "b")
}

func TestInstallDeps(t *testing.T) {
	tooSlow(t)
	tg := testgo(t)
	defer tg.cleanup()
	tg.parallel()
	tg.makeTempdir()
	tg.setenv("GOPATH", tg.tempdir)

	tg.tempFile("src/p1/p1.go", "package p1\nvar X =  1\n")
	tg.tempFile("src/p2/p2.go", "package p2\nimport _ \"p1\"\n")
	tg.tempFile("src/main1/main.go", "package main\nimport _ \"p2\"\nfunc main() {}\n")

	tg.run("list", "-f={{.Target}}", "p1")
	p1 := strings.TrimSpace(tg.getStdout())
	tg.run("list", "-f={{.Target}}", "p2")
	p2 := strings.TrimSpace(tg.getStdout())
	tg.run("list", "-f={{.Target}}", "main1")
	main1 := strings.TrimSpace(tg.getStdout())

	tg.run("install", "main1")

	tg.mustExist(main1)
	tg.mustNotExist(p2)
	tg.mustNotExist(p1)

	tg.run("install", "p2")
	tg.mustExist(p2)
	tg.mustNotExist(p1)

	// don't let install -i overwrite runtime
	tg.wantNotStale("runtime", "", "must be non-stale before install -i")

	tg.run("install", "-i", "main1")
	tg.mustExist(p1)
	tg.must(os.Remove(p1))

	tg.run("install", "-i", "p2")
	tg.mustExist(p1)
}

// Issue 22986.
func TestImportPath(t *testing.T) {
	tooSlow(t)
	tg := testgo(t)
	defer tg.cleanup()
	tg.parallel()

	tg.tempFile("src/a/a.go", `
package main

import (
	"log"
	p "a/p-1.0"
)

func main() {
	if !p.V {
		log.Fatal("false")
	}
}`)

	tg.tempFile("src/a/a_test.go", `
package main_test

import (
	p "a/p-1.0"
	"testing"
)

func TestV(t *testing.T) {
	if !p.V {
		t.Fatal("false")
	}
}`)

	tg.tempFile("src/a/p-1.0/p.go", `
package p

var V = true

func init() {}
`)

	tg.setenv("GOPATH", tg.path("."))
	tg.run("build", "-o", tg.path("a.exe"), "a")
	tg.run("test", "a")
}

func TestBadCommandLines(t *testing.T) {
	tg := testgo(t)
	defer tg.cleanup()
	tg.parallel()

	tg.tempFile("src/x/x.go", "package x\n")
	tg.setenv("GOPATH", tg.path("."))

	tg.run("build", "x")

	tg.tempFile("src/x/@y.go", "package x\n")
	tg.runFail("build", "x")
	tg.grepStderr("invalid input file name \"@y.go\"", "did not reject @y.go")
	tg.must(os.Remove(tg.path("src/x/@y.go")))

	tg.tempFile("src/x/-y.go", "package x\n")
	tg.runFail("build", "x")
	tg.grepStderr("invalid input file name \"-y.go\"", "did not reject -y.go")
	tg.must(os.Remove(tg.path("src/x/-y.go")))

	if runtime.Compiler == "gccgo" {
		tg.runFail("build", "-gccgoflags=all=@x", "x")
	} else {
		tg.runFail("build", "-gcflags=all=@x", "x")
	}
	tg.grepStderr("invalid command-line argument @x in command", "did not reject @x during exec")

	tg.tempFile("src/@x/x.go", "package x\n")
	tg.setenv("GOPATH", tg.path("."))
	tg.runFail("build", "@x")
	tg.grepStderr("invalid input directory name \"@x\"|cannot use path@version syntax", "did not reject @x directory")

	tg.tempFile("src/@x/y/y.go", "package y\n")
	tg.setenv("GOPATH", tg.path("."))
	tg.runFail("build", "@x/y")
	tg.grepStderr("invalid import path \"@x/y\"|cannot use path@version syntax", "did not reject @x/y import path")

	tg.tempFile("src/-x/x.go", "package x\n")
	tg.setenv("GOPATH", tg.path("."))
	tg.runFail("build", "--", "-x")
	tg.grepStderr("invalid input directory name \"-x\"", "did not reject -x directory")

	tg.tempFile("src/-x/y/y.go", "package y\n")
	tg.setenv("GOPATH", tg.path("."))
	tg.runFail("build", "--", "-x/y")
	tg.grepStderr("invalid import path \"-x/y\"", "did not reject -x/y import path")
}

func TestBadCgoDirectives(t *testing.T) {
	if !canCgo {
		t.Skip("no cgo")
	}
	tooSlow(t)
	tg := testgo(t)
	defer tg.cleanup()

	tg.tempFile("src/x/x.go", "package x\n")
	tg.setenv("GOPATH", tg.path("."))

	if runtime.Compiler == "gc" {
		tg.tempFile("src/x/x.go", `package x

			//go:cgo_ldflag "-fplugin=foo.so"

			import "C"
		`)
		tg.runFail("build", "x")
		tg.grepStderr("//go:cgo_ldflag .* only allowed in cgo-generated code", "did not reject //go:cgo_ldflag directive")
	}

	tg.must(os.Remove(tg.path("src/x/x.go")))
	tg.runFail("build", "x")
	tg.grepStderr("no Go files", "did not report missing source code")
	tg.tempFile("src/x/_cgo_yy.go", `package x

		//go:cgo_ldflag "-fplugin=foo.so"

		import "C"
	`)
	tg.runFail("build", "x")
	tg.grepStderr("no Go files", "did not report missing source code") // _* files are ignored...

	if runtime.Compiler == "gc" {
		tg.runFail("build", tg.path("src/x/_cgo_yy.go")) // ... but if forced, the comment is rejected
		// Actually, today there is a separate issue that _ files named
		// on the command line are ignored. Once that is fixed,
		// we want to see the cgo_ldflag error.
		tg.grepStderr("//go:cgo_ldflag only allowed in cgo-generated code|no Go files", "did not reject //go:cgo_ldflag directive")
	}

	tg.must(os.Remove(tg.path("src/x/_cgo_yy.go")))

	tg.tempFile("src/x/x.go", "package x\n")
	tg.tempFile("src/x/y.go", `package x
		// #cgo CFLAGS: -fplugin=foo.so
		import "C"
	`)
	tg.runFail("build", "x")
	tg.grepStderr("invalid flag in #cgo CFLAGS: -fplugin=foo.so", "did not reject -fplugin")

	tg.tempFile("src/x/y.go", `package x
		// #cgo CFLAGS: -Ibar -fplugin=foo.so
		import "C"
	`)
	tg.runFail("build", "x")
	tg.grepStderr("invalid flag in #cgo CFLAGS: -fplugin=foo.so", "did not reject -fplugin")

	tg.tempFile("src/x/y.go", `package x
		// #cgo pkg-config: -foo
		import "C"
	`)
	tg.runFail("build", "x")
	tg.grepStderr("invalid pkg-config package name: -foo", "did not reject pkg-config: -foo")

	tg.tempFile("src/x/y.go", `package x
		// #cgo pkg-config: @foo
		import "C"
	`)
	tg.runFail("build", "x")
	tg.grepStderr("invalid pkg-config package name: @foo", "did not reject pkg-config: -foo")

	tg.tempFile("src/x/y.go", `package x
		// #cgo CFLAGS: @foo
		import "C"
	`)
	tg.runFail("build", "x")
	tg.grepStderr("invalid flag in #cgo CFLAGS: @foo", "did not reject @foo flag")

	tg.tempFile("src/x/y.go", `package x
		// #cgo CFLAGS: -D
		import "C"
	`)
	tg.runFail("build", "x")
	tg.grepStderr("invalid flag in #cgo CFLAGS: -D without argument", "did not reject trailing -I flag")

	// Note that -I @foo is allowed because we rewrite it into -I /path/to/src/@foo
	// before the check is applied. There's no such rewrite for -D.

	tg.tempFile("src/x/y.go", `package x
		// #cgo CFLAGS: -D @foo
		import "C"
	`)
	tg.runFail("build", "x")
	tg.grepStderr("invalid flag in #cgo CFLAGS: -D @foo", "did not reject -D @foo flag")

	tg.tempFile("src/x/y.go", `package x
		// #cgo CFLAGS: -D@foo
		import "C"
	`)
	tg.runFail("build", "x")
	tg.grepStderr("invalid flag in #cgo CFLAGS: -D@foo", "did not reject -D@foo flag")

	tg.setenv("CGO_CFLAGS", "-D@foo")
	tg.tempFile("src/x/y.go", `package x
		import "C"
	`)
	tg.run("build", "-n", "x")
	tg.grepStderr("-D@foo", "did not find -D@foo in commands")
}

func TestTwoPkgConfigs(t *testing.T) {
	if !canCgo {
		t.Skip("no cgo")
	}
	if runtime.GOOS == "windows" || runtime.GOOS == "plan9" {
		t.Skipf("no shell scripts on %s", runtime.GOOS)
	}
	tooSlow(t)
	tg := testgo(t)
	defer tg.cleanup()
	tg.parallel()
	tg.tempFile("src/x/a.go", `package x
		// #cgo pkg-config: --static a
		import "C"
	`)
	tg.tempFile("src/x/b.go", `package x
		// #cgo pkg-config: --static a
		import "C"
	`)
	tg.tempFile("pkg-config.sh", `#!/bin/sh
echo $* >>`+tg.path("pkg-config.out"))
	tg.must(os.Chmod(tg.path("pkg-config.sh"), 0755))
	tg.setenv("GOPATH", tg.path("."))
	tg.setenv("PKG_CONFIG", tg.path("pkg-config.sh"))
	tg.run("build", "x")
	out, err := ioutil.ReadFile(tg.path("pkg-config.out"))
	tg.must(err)
	out = bytes.TrimSpace(out)
	want := "--cflags --static --static -- a a\n--libs --static --static -- a a"
	if !bytes.Equal(out, []byte(want)) {
		t.Errorf("got %q want %q", out, want)
	}
}

func TestCgoCache(t *testing.T) {
	if !canCgo {
		t.Skip("no cgo")
	}
	tooSlow(t)

	tg := testgo(t)
	defer tg.cleanup()
	tg.parallel()
	tg.tempFile("src/x/a.go", `package main
		// #ifndef VAL
		// #define VAL 0
		// #endif
		// int val = VAL;
		import "C"
		import "fmt"
		func main() { fmt.Println(C.val) }
	`)
	tg.setenv("GOPATH", tg.path("."))
	exe := tg.path("x.exe")
	tg.run("build", "-o", exe, "x")
	tg.setenv("CGO_LDFLAGS", "-lnosuchlibraryexists")
	tg.runFail("build", "-o", exe, "x")
	tg.grepStderr(`nosuchlibraryexists`, "did not run linker with changed CGO_LDFLAGS")
}

// Issue 23982
func TestFilepathUnderCwdFormat(t *testing.T) {
	tg := testgo(t)
	defer tg.cleanup()
	tg.parallel()
	tg.run("test", "-x", "-cover", "log")
	tg.grepStderrNot(`\.log\.cover\.go`, "-x output should contain correctly formatted filepath under cwd")
}

// Issue 24396.
func TestDontReportRemoveOfEmptyDir(t *testing.T) {
	tg := testgo(t)
	defer tg.cleanup()
	tg.parallel()
	tg.tempFile("src/a/a.go", `package a`)
	tg.setenv("GOPATH", tg.path("."))
	tg.run("install", "-x", "a")
	tg.run("install", "-x", "a")
	// The second install should have printed only a WORK= line,
	// nothing else.
	if bytes.Count(tg.stdout.Bytes(), []byte{'\n'})+bytes.Count(tg.stderr.Bytes(), []byte{'\n'}) > 1 {
		t.Error("unnecessary output when installing installed package")
	}
}

// Issue 24704.
func TestLinkerTmpDirIsDeleted(t *testing.T) {
	skipIfGccgo(t, "gccgo does not use cmd/link")
	if !canCgo {
		t.Skip("skipping because cgo not enabled")
	}
	tooSlow(t)

	tg := testgo(t)
	defer tg.cleanup()
	tg.parallel()
	tg.tempFile("a.go", `package main; import "C"; func main() {}`)
	tg.run("build", "-ldflags", "-v", "-o", os.DevNull, tg.path("a.go"))
	// Find line that has "host link:" in linker output.
	stderr := tg.getStderr()
	var hostLinkLine string
	for _, line := range strings.Split(stderr, "\n") {
		if !strings.Contains(line, "host link:") {
			continue
		}
		hostLinkLine = line
		break
	}
	if hostLinkLine == "" {
		t.Fatal(`fail to find with "host link:" string in linker output`)
	}
	// Find parameter, like "/tmp/go-link-408556474/go.o" inside of
	// "host link:" line, and extract temp directory /tmp/go-link-408556474
	// out of it.
	tmpdir := hostLinkLine
	i := strings.Index(tmpdir, `go.o"`)
	if i == -1 {
		t.Fatalf(`fail to find "go.o" in "host link:" line %q`, hostLinkLine)
	}
	tmpdir = tmpdir[:i-1]
	i = strings.LastIndex(tmpdir, `"`)
	if i == -1 {
		t.Fatalf(`fail to find " in "host link:" line %q`, hostLinkLine)
	}
	tmpdir = tmpdir[i+1:]
	// Verify that temp directory has been removed.
	_, err := os.Stat(tmpdir)
	if err == nil {
		t.Fatalf("temp directory %q has not been removed", tmpdir)
	}
	if !os.IsNotExist(err) {
		t.Fatalf("Stat(%q) returns unexpected error: %v", tmpdir, err)
	}
}

func testCDAndGOPATHAreDifferent(tg *testgoData, cd, gopath string) {
	skipIfGccgo(tg.t, "gccgo does not support -ldflags -X")
	tg.setenv("GOPATH", gopath)

	tg.tempDir("dir")
	exe := tg.path("dir/a.exe")

	tg.cd(cd)

	tg.run("build", "-o", exe, "-ldflags", "-X=my.pkg.Text=linkXworked")
	out, err := exec.Command(exe).CombinedOutput()
	if err != nil {
		tg.t.Fatal(err)
	}
	if string(out) != "linkXworked\n" {
		tg.t.Errorf(`incorrect output with GOPATH=%q and CD=%q: expected "linkXworked\n", but have %q`, gopath, cd, string(out))
	}
}

func TestCDAndGOPATHAreDifferent(t *testing.T) {
	tg := testgo(t)
	defer tg.cleanup()

	gopath := filepath.Join(tg.pwd(), "testdata")
	cd := filepath.Join(gopath, "src/my.pkg/main")

	testCDAndGOPATHAreDifferent(tg, cd, gopath)
	if runtime.GOOS == "windows" {
		testCDAndGOPATHAreDifferent(tg, cd, strings.ReplaceAll(gopath, `\`, `/`))
		testCDAndGOPATHAreDifferent(tg, cd, strings.ToUpper(gopath))
		testCDAndGOPATHAreDifferent(tg, cd, strings.ToLower(gopath))
	}
}

// Issue 25093.
func TestCoverpkgTestOnly(t *testing.T) {
	skipIfGccgo(t, "gccgo has no cover tool")
	tooSlow(t)
	tg := testgo(t)
	defer tg.cleanup()
	tg.parallel()
	tg.tempFile("src/a/a.go", `package a
		func F(i int) int {
			return i*i
		}`)
	tg.tempFile("src/atest/a_test.go", `
		package a_test
		import ( "a"; "testing" )
		func TestF(t *testing.T) { a.F(2) }
	`)
	tg.setenv("GOPATH", tg.path("."))
	tg.run("test", "-coverpkg=a", "atest")
	tg.grepStderrNot("no packages being tested depend on matches", "bad match message")
	tg.grepStdout("coverage: 100", "no coverage")
}
