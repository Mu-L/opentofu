// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2023 HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/opentofu/svchost"
	"github.com/opentofu/svchost/disco"
	"github.com/zclconf/go-cty/cty"

	"github.com/opentofu/opentofu/internal/addrs"
	backendInit "github.com/opentofu/opentofu/internal/backend/init"
	backendLocal "github.com/opentofu/opentofu/internal/backend/local"
	"github.com/opentofu/opentofu/internal/command/views"
	"github.com/opentofu/opentofu/internal/command/workdir"
	"github.com/opentofu/opentofu/internal/configs"
	"github.com/opentofu/opentofu/internal/configs/configload"
	"github.com/opentofu/opentofu/internal/configs/configschema"
	"github.com/opentofu/opentofu/internal/copy"
	"github.com/opentofu/opentofu/internal/depsfile"
	"github.com/opentofu/opentofu/internal/encryption"
	"github.com/opentofu/opentofu/internal/getproviders"
	"github.com/opentofu/opentofu/internal/initwd"
	legacy "github.com/opentofu/opentofu/internal/legacy/tofu"
	_ "github.com/opentofu/opentofu/internal/logging"
	"github.com/opentofu/opentofu/internal/plans"
	"github.com/opentofu/opentofu/internal/plans/planfile"
	"github.com/opentofu/opentofu/internal/providers"
	"github.com/opentofu/opentofu/internal/registry"
	"github.com/opentofu/opentofu/internal/states"
	"github.com/opentofu/opentofu/internal/states/statefile"
	"github.com/opentofu/opentofu/internal/states/statemgr"
	"github.com/opentofu/opentofu/internal/terminal"
	"github.com/opentofu/opentofu/internal/tofu"
	"github.com/opentofu/opentofu/version"
)

// These are the directories for our test data and fixtures.
var (
	fixtureDir  = "./testdata"
	testDataDir = "./testdata"
)

func init() {
	test = true

	// Initialize the backends
	backendInit.Init(nil)

	// Expand the data and fixture dirs on init because
	// we change the working directory in some tests.
	var err error
	fixtureDir, err = filepath.Abs(fixtureDir)
	if err != nil {
		panic(err)
	}

	testDataDir, err = filepath.Abs(testDataDir)
	if err != nil {
		panic(err)
	}
}

func TestMain(m *testing.M) {
	// Make sure backend init is initialized, since our tests tend to assume it.
	backendInit.Init(nil)

	os.Exit(m.Run())
}

// tempWorkingDir constructs a workdir.Dir object referring to a newly-created
// temporary directory. The temporary directory is automatically removed when
// the test and all its subtests complete.
//
// Although workdir.Dir is built to support arbitrary base directories, the
// not-yet-migrated behaviors in command.Meta tend to expect the root module
// directory to be the real process working directory, and so if you intend
// to use the result inside a command.Meta object you must use a pattern
// similar to the following when initializing your test:
//
//	wd := tempWorkingDir(t)
//	defer testChdir(t, wd.RootModuleDir())()
//
// Note that testChdir modifies global state for the test process, and so a
// test using this pattern must never call t.Parallel().
func tempWorkingDir(t *testing.T) *workdir.Dir {
	t.Helper()

	dirPath := t.TempDir()
	t.Logf("temporary directory %s", dirPath)

	return workdir.NewDir(dirPath)
}

// tempWorkingDirFixture is like tempWorkingDir but it also copies the content
// from a fixture directory into the temporary directory before returning it.
//
// The same caveats about working directory apply as for testWorkingDir. See
// the testWorkingDir commentary for an example of how to use this function
// along with testChdir to meet the expectations of command.Meta legacy
// functionality.
func tempWorkingDirFixture(t *testing.T, fixtureName string) *workdir.Dir {
	t.Helper()

	dirPath := testTempDirRealpath(t)
	t.Logf("temporary directory %s with fixture %q", dirPath, fixtureName)

	fixturePath := testFixturePath(fixtureName)
	testCopyDir(t, fixturePath, dirPath)
	// NOTE: Unfortunately because testCopyDir immediately aborts the test
	// on failure, a failure to copy will prevent us from cleaning up the
	// temporary directory. Oh well. :(

	return workdir.NewDir(dirPath)
}

func testFixturePath(name string) string {
	return filepath.Join(fixtureDir, name)
}

func metaOverridesForProvider(p providers.Interface) *testingOverrides {
	return &testingOverrides{
		Providers: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("test"):                                           providers.FactoryFixed(p),
			addrs.NewProvider(addrs.DefaultProviderRegistryHost, "hashicorp2", "test"): providers.FactoryFixed(p),
		},
	}
}

func testModuleWithSnapshot(t *testing.T, name string) (*configs.Config, *configload.Snapshot) {
	t.Helper()

	dir := filepath.Join(fixtureDir, name)
	loader := configload.NewLoaderForTests(t)

	// Test modules usually do not refer to remote sources, and for local
	// sources only this ultimately just records all of the module paths
	// in a JSON file so that we can load them below.
	inst := initwd.NewModuleInstaller(loader.ModulesDir(), loader, registry.NewClient(t.Context(), nil, nil), nil)
	_, instDiags := inst.InstallModules(context.Background(), dir, "tests", true, false, initwd.ModuleInstallHooksImpl{}, configs.RootModuleCallForTesting())
	if instDiags.HasErrors() {
		t.Fatal(instDiags.Err())
	}

	config, snap, diags := loader.LoadConfigWithSnapshot(t.Context(), dir, configs.RootModuleCallForTesting())
	if diags.HasErrors() {
		t.Fatal(diags.Error())
	}

	return config, snap
}

// testPlan returns a non-nil noop plan.
func testPlan(t *testing.T) *plans.Plan {
	t.Helper()

	// This is what an empty configuration block would look like after being
	// decoded with the schema of the "local" backend.
	backendConfig := cty.ObjectVal(map[string]cty.Value{
		"path":          cty.NullVal(cty.String),
		"workspace_dir": cty.NullVal(cty.String),
	})
	backendConfigRaw, err := plans.NewDynamicValue(backendConfig, backendConfig.Type())
	if err != nil {
		t.Fatal(err)
	}

	return &plans.Plan{
		Backend: plans.Backend{
			// This is just a placeholder so that the plan file can be written
			// out. Caller may wish to override it to something more "real"
			// where the plan will actually be subsequently applied.
			Type:   "local",
			Config: backendConfigRaw,
		},
		Changes: plans.NewChanges(),
	}
}

func testPlanFile(t *testing.T, configSnap *configload.Snapshot, state *states.State, plan *plans.Plan) string {
	return testPlanFileMatchState(t, configSnap, state, plan, statemgr.SnapshotMeta{})
}

func testPlanFileMatchState(t *testing.T, configSnap *configload.Snapshot, state *states.State, plan *plans.Plan, stateMeta statemgr.SnapshotMeta) string {
	t.Helper()

	stateFile := &statefile.File{
		Lineage:          stateMeta.Lineage,
		Serial:           stateMeta.Serial,
		State:            state,
		TerraformVersion: version.SemVer,
	}
	prevStateFile := &statefile.File{
		Lineage:          stateMeta.Lineage,
		Serial:           stateMeta.Serial,
		State:            state, // we just assume no changes detected during refresh
		TerraformVersion: version.SemVer,
	}

	path := testTempFile(t)
	err := planfile.Create(path, planfile.CreateArgs{
		ConfigSnapshot:       configSnap,
		PreviousRunStateFile: prevStateFile,
		StateFile:            stateFile,
		Plan:                 plan,
		DependencyLocks:      depsfile.NewLocks(),
	}, encryption.PlanEncryptionDisabled())
	if err != nil {
		t.Fatalf("failed to create temporary plan file: %s", err)
	}

	return path
}

// testPlanFileNoop is a shortcut function that creates a plan file that
// represents no changes and returns its path. This is useful when a test
// just needs any plan file, and it doesn't matter what is inside it.
func testPlanFileNoop(t *testing.T) string {
	snap := &configload.Snapshot{
		Modules: map[string]*configload.SnapshotModule{
			"": {
				Dir: ".",
				Files: map[string][]byte{
					"main.tf": nil,
				},
			},
		},
	}
	state := states.NewState()
	plan := testPlan(t)
	return testPlanFile(t, snap, state, plan)
}

func testFileEquals(t *testing.T, got, want string) {
	t.Helper()

	actual, err := os.ReadFile(got)
	if err != nil {
		t.Fatalf("error reading %s", got)
	}

	expected, err := os.ReadFile(want)
	if err != nil {
		t.Fatalf("error reading %s", want)
	}

	if diff := cmp.Diff(string(actual), string(expected)); len(diff) > 0 {
		t.Fatalf("got:\n%s\nwant:\n%s\ndiff:\n%s", actual, expected, diff)
	}
}

func testReadPlan(t *testing.T, path string) *plans.Plan {
	t.Helper()

	f, err := planfile.Open(path, encryption.PlanEncryptionDisabled())
	if err != nil {
		t.Fatalf("error opening plan file %q: %s", path, err)
	}

	p, err := f.ReadPlan()
	if err != nil {
		t.Fatalf("error reading plan from plan file %q: %s", path, err)
	}

	return p
}

// testState returns a test State structure that we use for a lot of tests.
func testState() *states.State {
	return states.BuildState(func(s *states.SyncState) {
		s.SetResourceInstanceCurrent(
			addrs.Resource{
				Mode: addrs.ManagedResourceMode,
				Type: "test_instance",
				Name: "foo",
			}.Instance(addrs.NoKey).Absolute(addrs.RootModuleInstance),
			&states.ResourceInstanceObjectSrc{
				// The weird whitespace here is reflective of how this would
				// get written out in a real state file, due to the indentation
				// of all of the containing wrapping objects and arrays.
				AttrsJSON:    []byte(`{"id":"bar"}`),
				Status:       states.ObjectReady,
				Dependencies: []addrs.ConfigResource{},
			},
			addrs.AbsProviderConfig{
				Provider: addrs.NewDefaultProvider("test"),
				Module:   addrs.RootModule,
			},
			addrs.NoKey,
		)
		// DeepCopy is used here to ensure our synthetic state matches exactly
		// with a state that will have been copied during the command
		// operation, and all fields have been copied correctly.
	}).DeepCopy()
}

// writeStateForTesting is a helper that writes the given naked state to the
// given writer, generating a stub *statefile.File wrapper which is then
// immediately discarded.
func writeStateForTesting(state *states.State, w io.Writer) error {
	sf := &statefile.File{
		Serial:  0,
		Lineage: "fake-for-testing",
		State:   state,
	}
	return statefile.Write(sf, w, encryption.StateEncryptionDisabled())
}

// testStateMgrCurrentLineage returns the current lineage for the given state
// manager, or the empty string if it does not use lineage. This is primarily
// for testing against the local backend, which always supports lineage.
func testStateMgrCurrentLineage(mgr statemgr.Persistent) string {
	if pm, ok := mgr.(statemgr.PersistentMeta); ok {
		m := pm.StateSnapshotMeta()
		return m.Lineage
	}
	return ""
}

// markStateForMatching is a helper that writes a specific marker value to
// a state so that it can be recognized later with getStateMatchingMarker.
//
// Internally this just sets a root module output value called "testing_mark"
// to the given string value. If the state is being checked in other ways,
// the test code may need to compensate for the addition or overwriting of this
// special output value name.
//
// The given mark string is returned verbatim, to allow the following pattern
// in tests:
//
//	mark := markStateForMatching(state, "foo")
//	// (do stuff to the state)
//	assertStateHasMarker(state, mark)
func markStateForMatching(state *states.State, mark string) string {
	state.RootModule().SetOutputValue("testing_mark", cty.StringVal(mark), false, "")
	return mark
}

// getStateMatchingMarker is used with markStateForMatching to retrieve the
// mark string previously added to the given state. If no such mark is present,
// the result is an empty string.
func getStateMatchingMarker(state *states.State) string {
	os := state.RootModule().OutputValues["testing_mark"]
	if os == nil {
		return ""
	}
	v := os.Value
	if v.Type() == cty.String && v.IsKnown() && !v.IsNull() {
		return v.AsString()
	}
	return ""
}

// stateHasMarker is a helper around getStateMatchingMarker that also includes
// the equality test, for more convenient use in test assertion branches.
func stateHasMarker(state *states.State, want string) bool {
	return getStateMatchingMarker(state) == want
}

// assertStateHasMarker wraps stateHasMarker to automatically generate a
// fatal test result (i.e. t.Fatal) if the marker doesn't match.
func assertStateHasMarker(t *testing.T, state *states.State, want string) {
	if !stateHasMarker(state, want) {
		t.Fatalf("wrong state marker\ngot:  %q\nwant: %q", getStateMatchingMarker(state), want)
	}
}

func testStateFile(t *testing.T, s *states.State) string {
	t.Helper()

	path := testTempFile(t)

	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("failed to create temporary state file %s: %s", path, err)
	}
	defer f.Close()

	err = writeStateForTesting(s, f)
	if err != nil {
		t.Fatalf("failed to write state to temporary file %s: %s", path, err)
	}

	return path
}

// testStateFileDefault writes the state out to the default statefile
// in the cwd. Use `testCwd` to change into a temp cwd.
func testStateFileDefault(t *testing.T, s *states.State) {
	t.Helper()

	f, err := os.Create(DefaultStateFilename)
	if err != nil {
		t.Fatalf("err: %s", err)
	}
	defer f.Close()

	if err := writeStateForTesting(s, f); err != nil {
		t.Fatalf("err: %s", err)
	}
}

// testStateFileWorkspaceDefault writes the state out to the default statefile
// for the given workspace in the cwd. Use `testCwd` to change into a temp cwd.
func testStateFileWorkspaceDefault(t *testing.T, workspace string, s *states.State) string {
	t.Helper()

	workspaceDir := filepath.Join(backendLocal.DefaultWorkspaceDir, workspace)
	err := os.MkdirAll(workspaceDir, os.ModePerm)
	if err != nil {
		t.Fatalf("err: %s", err)
	}

	path := filepath.Join(workspaceDir, DefaultStateFilename)
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("err: %s", err)
	}
	defer f.Close()

	if err := writeStateForTesting(s, f); err != nil {
		t.Fatalf("err: %s", err)
	}

	return path
}

// testStateFileRemote writes the state out to the remote statefile
// in the cwd. Use `testCwd` to change into a temp cwd.
func testStateFileRemote(t *testing.T, s *legacy.State) string {
	t.Helper()

	path := filepath.Join(DefaultDataDir, DefaultStateFilename)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("err: %s", err)
	}

	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("err: %s", err)
	}
	defer f.Close()

	if err := legacy.WriteState(s, f); err != nil {
		t.Fatalf("err: %s", err)
	}

	return path
}

// testStateRead reads the state from a file
func testStateRead(t *testing.T, path string) *states.State {
	t.Helper()

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("err: %s", err)
	}
	defer f.Close()

	sf, err := statefile.Read(f, encryption.StateEncryptionDisabled())
	if err != nil {
		t.Fatalf("err: %s", err)
	}

	return sf.State
}

// testDataStateRead reads a "data state", which is a file format resembling
// our state format v3 that is used only to track current backend settings.
//
// This old format still uses *legacy.State, but should be replaced with
// a more specialized type in a later release.
func testDataStateRead(t *testing.T, path string) *legacy.State {
	t.Helper()

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("err: %s", err)
	}
	defer f.Close()

	s, err := legacy.ReadState(f)
	if err != nil {
		t.Fatalf("err: %s", err)
	}

	return s
}

// testStateOutput tests that the state at the given path contains
// the expected state string.
func testStateOutput(t *testing.T, path string, expected string) {
	t.Helper()

	newState := testStateRead(t, path)
	actual := strings.TrimSpace(newState.String())
	expected = strings.TrimSpace(expected)
	if actual != expected {
		t.Fatalf("expected:\n%s\nactual:\n%s", expected, actual)
	}
}

func testProvider() *tofu.MockProvider {
	p := new(tofu.MockProvider)
	p.PlanResourceChangeFn = func(req providers.PlanResourceChangeRequest) (resp providers.PlanResourceChangeResponse) {
		resp.PlannedState = req.ProposedNewState
		return resp
	}

	p.ReadResourceFn = func(req providers.ReadResourceRequest) providers.ReadResourceResponse {
		return providers.ReadResourceResponse{
			NewState: req.PriorState,
		}
	}
	return p
}

func testTempFile(t *testing.T) string {
	t.Helper()

	return filepath.Join(testTempDirRealpath(t), "state.tfstate")
}

// testTempDirRealpath is like [testing.T.TempDir] but takes the
// extra step of ensuring that the result is a path that does not
// include any symlinks.
func testTempDirRealpath(t *testing.T) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// testCwdTemp is used to change the current working directory into a temporary
// directory. The cleanup is performed automatically after the test and all its
// subtests complete.
func testCwdTemp(t testing.TB) string {
	t.Helper()

	tmp := t.TempDir()
	t.Chdir(tmp)
	return tmp
}

// testStdinPipe changes os.Stdin to be a pipe that sends the data from
// the reader before closing the pipe.
//
// The returned function should be deferred to properly clean up and restore
// the original stdin.
func testStdinPipe(t *testing.T, src io.Reader) func() {
	t.Helper()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("err: %s", err)
	}

	// Modify stdin to point to our new pipe
	old := os.Stdin
	os.Stdin = r

	// Copy the data from the reader to the pipe
	go func() {
		defer w.Close()
		if _, err := io.Copy(w, src); err != nil {
			panic(err)
		}
	}()

	return func() {
		// Close our read end
		r.Close()

		// Reset stdin
		os.Stdin = old
	}
}

// Modify os.Stdout to write to the given buffer. Note that this is generally
// not useful since the commands are configured to write to a cli.Ui, not
// Stdout directly. Commands like `console` though use the raw stdout.
func testStdoutCapture(t *testing.T, dst io.Writer) func() {
	t.Helper()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("err: %s", err)
	}

	// Modify stdout
	old := os.Stdout
	os.Stdout = w

	// Copy
	doneCh := make(chan struct{})
	go func() {
		defer close(doneCh)
		if _, err := io.Copy(dst, r); err != nil {
			panic(err)
		}
		if err := r.Close(); err != nil {
			panic(err)
		}
	}()

	return func() {
		// Close the writer end of the pipe
		// This test code is racey
		_ = w.Sync()
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}

		// Reset stdout
		os.Stdout = old

		// Wait for the data copy to complete to avoid a race reading data
		<-doneCh
	}
}

// testInteractiveInput configures tests so that the answers given are sent
// in order to interactive prompts. The returned function must be called
// in a defer to clean up.
func testInteractiveInput(t *testing.T, answers []string) func() {
	t.Helper()

	// Disable test mode so input is called
	test = false

	// Set up reader/writers
	testInputResponse = answers
	defaultInputReader = bytes.NewBufferString("")
	defaultInputWriter = new(bytes.Buffer)

	// Return the cleanup
	return func() {
		test = true
		testInputResponse = nil
	}
}

// testInputMap configures tests so that the given answers are returned
// for calls to Input when the right question is asked. The key is the
// question "Id" that is used.
func testInputMap(t *testing.T, answers map[string]string) func() {
	t.Helper()

	// Disable test mode so input is called
	test = false

	// Set up reader/writers
	defaultInputReader = bytes.NewBufferString("")
	defaultInputWriter = new(bytes.Buffer)

	// Setup answers
	testInputResponse = nil
	testInputResponseMap = answers

	// Return the cleanup
	return func() {
		var unusedAnswers = testInputResponseMap

		// First, clean up!
		test = true
		testInputResponseMap = nil

		if len(unusedAnswers) > 0 {
			t.Fatalf("expected no unused answers provided to command.testInputMap, got: %v", unusedAnswers)
		}
	}
}

// testBackendState is used to make a test HTTP server to test a configured
// backend. This returns the complete state that can be saved. Use
// `testStateFileRemote` to write the returned state.
//
// When using this function, the configuration fixture for the test must
// include an empty configuration block for the HTTP backend, like this:
//
//	terraform {
//	  backend "http" {
//	  }
//	}
//
// If such a block isn't present, or if it isn't empty, then an error will
// be returned about the backend configuration having changed and that
// "tofu init" must be run, since the test backend config cache created
// by this function contains the hash for an empty configuration.
func testBackendState(t *testing.T, s *states.State, c int) (*legacy.State, *httptest.Server) {
	t.Helper()

	var b64md5 string
	buf := bytes.NewBuffer(nil)

	cb := func(resp http.ResponseWriter, req *http.Request) {
		if req.Method == "PUT" {
			resp.WriteHeader(c)
			return
		}
		if s == nil {
			resp.WriteHeader(404)
			return
		}

		resp.Header().Set("Content-MD5", b64md5)
		if _, err := resp.Write(buf.Bytes()); err != nil {
			t.Fatal(err)
		}
	}

	// If a state was given, make sure we calculate the proper b64md5
	if s != nil {
		err := statefile.Write(&statefile.File{State: s}, buf, encryption.StateEncryptionDisabled())
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		md5 := md5.Sum(buf.Bytes())
		b64md5 = base64.StdEncoding.EncodeToString(md5[:16])
	}

	srv := httptest.NewServer(http.HandlerFunc(cb))

	backendConfig := &configs.Backend{
		Type:   "http",
		Config: configs.SynthBody("<testBackendState>", map[string]cty.Value{}),
		Eval:   configs.NewStaticEvaluator(nil, configs.RootModuleCallForTesting()),
	}
	b := backendInit.Backend("http")(encryption.StateEncryptionDisabled())
	configSchema := b.ConfigSchema()
	hash, _ := backendConfig.Hash(t.Context(), configSchema)

	state := legacy.NewState()
	state.Backend = &legacy.BackendState{
		Type:      "http",
		ConfigRaw: json.RawMessage(fmt.Sprintf(`{"address":%q}`, srv.URL)),
		Hash:      uint64(hash),
	}

	return state, srv
}

// testRemoteState is used to make a test HTTP server to return a given
// state file that can be used for testing legacy remote state.
//
// The return values are a *legacy.State instance that should be written
// as the "data state" (really: backend state) and the server that the
// returned data state refers to.
func testRemoteState(t *testing.T, s *states.State, c int) (*legacy.State, *httptest.Server) {
	t.Helper()

	var b64md5 string
	buf := bytes.NewBuffer(nil)

	cb := func(resp http.ResponseWriter, req *http.Request) {
		if req.Method == "PUT" {
			resp.WriteHeader(c)
			return
		}
		if s == nil {
			resp.WriteHeader(404)
			return
		}

		resp.Header().Set("Content-MD5", b64md5)
		if _, err := resp.Write(buf.Bytes()); err != nil {
			t.Fatal(err)
		}
	}

	retState := legacy.NewState()

	srv := httptest.NewServer(http.HandlerFunc(cb))
	b := &legacy.BackendState{
		Type: "http",
	}
	if err := b.SetConfig(cty.ObjectVal(map[string]cty.Value{
		"address": cty.StringVal(srv.URL),
	}), &configschema.Block{
		Attributes: map[string]*configschema.Attribute{
			"address": {
				Type:     cty.String,
				Required: true,
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	retState.Backend = b

	if s != nil {
		err := statefile.Write(&statefile.File{State: s}, buf, encryption.StateEncryptionDisabled())
		if err != nil {
			t.Fatalf("failed to write initial state: %v", err)
		}
	}

	return retState, srv
}

// testLockState calls a separate process to the lock the state file at path.
// deferFunc should be called in the caller to properly unlock the file.
// Since many tests change the working directory, the sourceDir argument must be
// supplied to locate the statelocker.go source.
func testLockState(t *testing.T, sourceDir, path string) (func(), error) {
	// build and run the binary ourselves so we can quickly terminate it for cleanup
	buildDir := t.TempDir()

	source := filepath.Join(sourceDir, "statelocker.go")
	lockBin := filepath.Join(buildDir, "statelocker")

	cmd := exec.Command("go", "build", "-o", lockBin, source)
	cmd.Dir = filepath.Dir(sourceDir)

	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("%w %s", err, out)
	}

	locker := exec.Command(lockBin, path)
	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	defer pr.Close()
	defer pw.Close()
	locker.Stderr = pw
	locker.Stdout = pw

	if err := locker.Start(); err != nil {
		return nil, err
	}
	deferFunc := func() {
		if err := locker.Process.Signal(syscall.SIGTERM); err != nil {
			t.Fatal(err)
		}
		// Assume the sigterm above succeeds. The error here may represent
		// the signal sent above, but is difficult to check in a platform
		// agostic way
		_ = locker.Wait()
	}

	// wait for the process to lock
	buf := make([]byte, 1024)
	n, err := pr.Read(buf)
	if err != nil {
		return deferFunc, fmt.Errorf("read from statelocker returned: %w", err)
	}

	output := string(buf[:n])
	if !strings.HasPrefix(output, "LOCKID") {
		return deferFunc, fmt.Errorf("statelocker wrote: %s", string(buf[:n]))
	}
	return deferFunc, nil
}

// testCopyDir recursively copies a directory tree, attempting to preserve
// permissions. Source directory must exist, destination directory may exist
// but will be created if not; it should typically be a temporary directory,
// and thus already created using os.MkdirTemp or similar.
// Symlinks are ignored and skipped.
func testCopyDir(t *testing.T, src, dst string) {
	t.Helper()

	src = filepath.Clean(src)
	dst = filepath.Clean(dst)

	si, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}
	if !si.IsDir() {
		t.Fatal("source is not a directory")
	}

	_, err = os.Stat(dst)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}

	err = os.MkdirAll(dst, si.Mode())
	if err != nil {
		t.Fatal(err)
	}

	entries, err := os.ReadDir(src)
	if err != nil {
		return
	}

	for _, entry := range entries {
		srcPath := filepath.Join(src, entry.Name())
		dstPath := filepath.Join(dst, entry.Name())

		// If the entry is a symlink, we copy the contents
		for entry.Type()&os.ModeSymlink != 0 {
			target, err := os.Readlink(srcPath)
			if err != nil {
				t.Fatal(err)
			}

			fi, err := os.Stat(target)
			if err != nil {
				t.Fatal(err)
			}
			entry = fs.FileInfoToDirEntry(fi)
		}

		if entry.IsDir() {
			testCopyDir(t, srcPath, dstPath)
		} else {
			err = copy.CopyFile(srcPath, dstPath)
			if err != nil {
				t.Fatal(err)
			}
		}
	}
}

// normalizeJSON removes all insignificant whitespace from the given JSON buffer
// and returns it as a string for easier comparison.
func normalizeJSON(t *testing.T, src []byte) string {
	t.Helper()
	var buf bytes.Buffer
	err := json.Compact(&buf, src)
	if err != nil {
		t.Fatalf("error normalizing JSON: %s", err)
	}
	return buf.String()
}

func mustResourceAddr(s string) addrs.ConfigResource {
	addr, diags := addrs.ParseAbsResourceStr(s)
	if diags.HasErrors() {
		panic(diags.Err())
	}
	return addr.Config()
}

// This map from provider type name to namespace is used by the fake registry
// when called via LookupLegacyProvider. Providers not in this map will return
// a 404 Not Found error.
var legacyProviderNamespaces = map[string]string{
	"foo": "hashicorp",
	"bar": "hashicorp",
	"baz": "terraform-providers",
	"qux": "hashicorp",
}

// This map is used to mock the provider redirect feature.
var movedProviderNamespaces = map[string]string{
	"qux": "acme",
}

// testServices starts up a local HTTP server running a fake provider registry
// service which responds only to discovery requests and legacy provider lookup
// API calls.
//
// The final return value is a function to call at the end of a test function
// to shut down the test server. After you call that function, the discovery
// object becomes useless.
func testServices(t *testing.T) (services *disco.Disco, cleanup func()) {
	server := httptest.NewServer(http.HandlerFunc(fakeRegistryHandler))

	services = disco.New()
	services.ForceHostServices(svchost.Hostname("registry.opentofu.org"), map[string]interface{}{
		"providers.v1": server.URL + "/providers/v1/",
	})

	return services, func() {
		server.Close()
	}
}

// testRegistrySource is a wrapper around testServices that uses the created
// discovery object to produce a Source instance that is ready to use with the
// fake registry services.
//
// As with testServices, the final return value is a function to call at the end
// of your test in order to shut down the test server.
func testRegistrySource(t *testing.T) (source *getproviders.RegistrySource, cleanup func()) {
	services, close := testServices(t)
	source = getproviders.NewRegistrySource(t.Context(), services, nil)
	return source, close
}

func fakeRegistryHandler(resp http.ResponseWriter, req *http.Request) {
	path := req.URL.EscapedPath()

	write := func(data string) {
		if _, err := resp.Write([]byte(data)); err != nil {
			panic(err)
		}
	}

	if !strings.HasPrefix(path, "/providers/v1/") {
		resp.WriteHeader(404)
		write(`not a provider registry endpoint`)
		return
	}

	pathParts := strings.Split(path, "/")[3:]

	if len(pathParts) != 3 {
		resp.WriteHeader(404)
		write(`unrecognized path scheme`)
		return
	}

	if pathParts[2] != "versions" {
		resp.WriteHeader(404)
		write(`this registry only supports legacy namespace lookup requests`)
		return
	}

	name := pathParts[1]

	// Legacy lookup
	if pathParts[0] == "-" {
		if namespace, ok := legacyProviderNamespaces[name]; ok {
			resp.Header().Set("Content-Type", "application/json")
			resp.WriteHeader(200)
			if movedNamespace, ok := movedProviderNamespaces[name]; ok {
				fmt.Fprintf(resp, `{"id":"%s/%s","moved_to":"%s/%s","versions":[{"version":"1.0.0","protocols":["4"]}]}`, namespace, name, movedNamespace, name)
			} else {
				fmt.Fprintf(resp, `{"id":"%s/%s","versions":[{"version":"1.0.0","protocols":["4"]}]}`, namespace, name)
			}
		} else {
			resp.WriteHeader(404)
			write(`provider not found`)
		}
		return
	}

	// Also return versions for redirect target
	if namespace, ok := movedProviderNamespaces[name]; ok && pathParts[0] == namespace {
		resp.Header().Set("Content-Type", "application/json")
		resp.WriteHeader(200)
		fmt.Fprintf(resp, `{"id":"%s/%s","versions":[{"version":"1.0.0","protocols":["4"]}]}`, namespace, name)
	} else {
		resp.WriteHeader(404)
		write(`provider not found`)
	}
}

func testView(t *testing.T) (*views.View, func(*testing.T) *terminal.TestOutput) {
	streams, done := terminal.StreamsForTesting(t)
	return views.NewView(streams), done
}

// checkGoldenReference compares the given test output with a known "golden" output log
// located under the specified fixture path.
//
// If any of these tests fail, please communicate with Terraform Cloud folks before resolving,
// as changes to UI output may also affect the behavior of Terraform Cloud's structured run output.
func checkGoldenReference(t *testing.T, output *terminal.TestOutput, fixturePathName string) {
	t.Helper()

	// Load the golden reference fixture
	wantFile, err := os.Open(path.Join(testFixturePath(fixturePathName), "output.jsonlog"))
	if err != nil {
		t.Fatalf("failed to open output file: %s", err)
	}
	defer wantFile.Close()
	wantBytes, err := io.ReadAll(wantFile)
	if err != nil {
		t.Fatalf("failed to read output file: %s", err)
	}
	want := string(wantBytes)

	got := output.Stdout()

	// Split the output and the reference into lines so that we can compare
	// messages
	got = strings.TrimSuffix(got, "\n")
	gotLines := strings.Split(got, "\n")

	want = strings.TrimSuffix(want, "\n")
	wantLines := strings.Split(want, "\n")

	if len(gotLines) != len(wantLines) {
		t.Errorf("unexpected number of log lines: got %d, want %d\n"+
			"NOTE: This failure may indicate a UI change affecting the behavior of structured run output on TFC.\n"+
			"Please communicate with Terraform Cloud team before resolving", len(gotLines), len(wantLines))
	}

	// Verify that the log starts with a version message
	type versionMessage struct {
		Level    string `json:"@level"`
		Message  string `json:"@message"`
		Type     string `json:"type"`
		OpenTofu string `json:"tofu"`
		UI       string `json:"ui"`
	}
	var gotVersion versionMessage
	if err := json.Unmarshal([]byte(gotLines[0]), &gotVersion); err != nil {
		t.Errorf("failed to unmarshal version line: %s\n%s", err, gotLines[0])
	}
	wantVersion := versionMessage{
		"info",
		fmt.Sprintf("OpenTofu %s", version.String()),
		"version",
		version.String(),
		views.JSON_UI_VERSION,
	}
	if !cmp.Equal(wantVersion, gotVersion) {
		t.Errorf("unexpected first message:\n%s", cmp.Diff(wantVersion, gotVersion))
	}

	// Compare the rest of the lines against the golden reference
	var gotLineMaps []map[string]interface{}
	for i, line := range gotLines[1:] {
		index := i + 1
		var gotMap map[string]interface{}
		if err := json.Unmarshal([]byte(line), &gotMap); err != nil {
			t.Errorf("failed to unmarshal got line %d: %s\n%s", index, err, gotLines[index])
		}
		if _, ok := gotMap["@timestamp"]; !ok {
			t.Errorf("missing @timestamp field in log: %s", gotLines[index])
		}
		gotMap = deleteMapField(gotMap, "hook", "elapsed_seconds")
		delete(gotMap, "@timestamp")
		gotLineMaps = append(gotLineMaps, gotMap)
	}

	var wantLineMaps []map[string]interface{}
	for i, line := range wantLines[1:] {
		index := i + 1
		var wantMap map[string]interface{}
		if err := json.Unmarshal([]byte(line), &wantMap); err != nil {
			t.Errorf("failed to unmarshal want line %d: %s\n%s", index, err, gotLines[index])
		}
		wantMap = deleteMapField(wantMap, "hook", "elapsed_seconds")
		wantLineMaps = append(wantLineMaps, wantMap)
	}

	if diff := cmp.Diff(wantLineMaps, gotLineMaps); diff != "" {
		t.Errorf("wrong output lines\n%s\n"+
			"NOTE: This failure may indicate a UI change affecting the behavior of structured run output on TFC.\n"+
			"Please communicate with Terraform Cloud team before resolving", diff)
	}
}

func deleteMapField(fieldMap map[string]interface{}, rootField, field string) map[string]interface{} {
	rootMap, ok := fieldMap[rootField].(map[string]interface{})
	if !ok {
		return fieldMap
	}

	delete(rootMap, field)
	return rootMap
}

// testHangServer starts a local HTTP server that accepts incoming requests
// but then intentionally leaves the connection hanging without responding,
// writing the request to the returned channel so that the caller can then
// trigger some mechanism for cancelling that hung request.
//
// This is intended for testing anything that needs to be able to cancel
// slow requests to remote HTTP servers, so that the test can be sure that
// the request definitely will be "slow enough" that cancellation is
// definitely the only way the request could've halted.
//
// The returned server is automatically closed when the calling test
// is complete, but the caller is also allowed to optionally call Close
// directly itself. Note that the Close method alone will not close
// any active requests, but testHangServer guarantees that it will
// eventually terminate active requests once the calling test is
// complete.
func testHangServer(t testing.TB) (server *httptest.Server, reqs <-chan *http.Request) {
	t.Helper()

	// We'll use this channel to signal any active requests to terminate
	// during test cleanup, so that the active requests can't remain
	// running indefinitely.
	cleanupCh := make(chan struct{})

	// This channel is how we'll notify the caller when we get a request.
	// This has a buffer so that in the assumed-typical case where the
	// test server will only start serving a few requests before they
	// get cancelled the server's handler can be decoupled from the
	// channel reads in the caller.
	reqsCh := make(chan *http.Request, 8)

	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		// We intentionally don't take any action on this request until
		// the test cleanup function runs, but we will notify our
		// caller that the request was started.
		//
		// We'll also accept getting told to clean up before the
		// channel write succeeds just in case the calling test exits
		// before it reads from reqsCh.
		select {
		case reqsCh <- req:
		case <-cleanupCh:
		}
		// If we managed to send req to reqsCh above then we still
		// need to wait for cleanupCh to close. The following is
		// no-op if the channel is already closed.
		<-cleanupCh
		// If any client is still connected by the time we get here then
		// we'll respond quickly just to get their connection closed.
		// This is unlikely but could potentially happen if a new client
		// connects in the narrow time window between us closing the
		// existing client connections and fully closing the server,
		// after cleanupCh is already closed: in that case the new client
		// will get a 500 Internal Server Error response immediately.
		w.WriteHeader(500)
	}))
	t.Logf("testHangServer is running at %s", server.URL)

	t.Cleanup(func() {
		t.Helper()
		t.Log("shutting down testHangServer")
		close(cleanupCh)                // terminate any active handlers
		close(reqsCh)                   // unblock any test that's awaiting a request notification
		server.CloseClientConnections() // force any active clients to disconnect
		server.Close()                  // stop accepting new requests and wait for existing ones to stop
	})
	return server, reqsCh
}
