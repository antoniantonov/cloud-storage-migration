package runner

import (
	"bufio"
	"bytes"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/antoniantonov/cloud-storage-migration/src/internal/config"
)

var configVariables = []string{
	"LIST_ALL", "COPY_ITEM_IDS", "MICROSOFT_CLIENT_ID",
	"GOOGLE_CLIENT_CREDENTIALS_FILE", "AUTH_DIR", "TEMP_COPY_DIR",
	"AUTH_INTERACTIVE", "GOOGLE_OAUTH_PORT", "GOOGLE_OAUTH_LISTEN_HOST", "DOCKER_IMAGE",
}

type invocation struct {
	Tool string
	Args []string
	CWD  string
	Env  map[string]string
	PID  int
}

// Fake executables reenter this test binary, avoiding a second compiler or
// platform-specific argument logging tools in the shell fixtures.
func TestRunnerHelperProcess(t *testing.T) {
	if os.Getenv("RUNNER_HELPER") != "1" {
		return
	}
	var args []string
	for i, arg := range os.Args {
		if arg == "--" {
			args = os.Args[i+1:]
			break
		}
	}
	if len(args) == 0 {
		os.Exit(99)
	}
	tool := args[0]
	args = args[1:]
	if tool == "id" {
		name, fallback := "FAKE_UID", "1001"
		if len(args) > 0 && args[0] == "-g" {
			name, fallback = "FAKE_GID", "1002"
		}
		value := os.Getenv(name)
		if value == "" {
			value = fallback
		}
		fmt.Println(value)
		os.Exit(0)
	}
	cwd, err := os.Getwd()
	if err != nil {
		panic(err)
	}
	call := invocation{Tool: tool, Args: args, CWD: cwd, Env: make(map[string]string), PID: os.Getpid()}
	for _, name := range configVariables {
		if value, ok := os.LookupEnv(name); ok {
			call.Env[name] = value
		}
	}
	file, err := os.OpenFile(os.Getenv("RUNNER_LOG"), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0600)
	if err != nil {
		panic(err)
	}
	if err := json.NewEncoder(file).Encode(call); err != nil {
		panic(err)
	}
	if err := file.Close(); err != nil {
		panic(err)
	}
	exit := func(name string) {
		status, _ := strconv.Atoi(os.Getenv(name))
		os.Exit(status)
	}
	switch tool {
	case "go":
		fmt.Print(os.Getenv("FAKE_GO_STDOUT"))
		if os.Getenv("FAKE_GO_STATUS") != "" && os.Getenv("FAKE_GO_STATUS") != "0" {
			exit("FAKE_GO_STATUS")
		}
		for i := range args {
			if args[i] == "-o" && i+1 < len(args) {
				if err := os.WriteFile(args[i+1], helperScript("app"), 0755); err != nil {
					panic(err)
				}
				exit("FAKE_GO_STATUS")
			}
		}
		os.Exit(98)
	case "app":
		fmt.Print(os.Getenv("FAKE_APP_STDOUT"))
		exit("FAKE_APP_STATUS")
	case "docker":
		if len(args) > 0 {
			switch args[0] {
			case "build":
				fmt.Print(os.Getenv("FAKE_BUILD_STDOUT"))
				exit("FAKE_BUILD_STATUS")
			case "image":
				exit("FAKE_INSPECT_STATUS")
			case "run":
				fmt.Print(os.Getenv("FAKE_RUN_STDOUT"))
				exit("FAKE_RUN_STATUS")
			}
		}
	}
	os.Exit(97)
}

func helperScript(tool string) []byte {
	return []byte("#!/bin/bash\nexec \"$RUNNER_TEST_BINARY\" -test.run=^TestRunnerHelperProcess$ -- " + tool + " \"$@\"\n")
}

type fixture struct {
	root string
	cwd  string
	log  string
	env  []string
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	// Keep scratch data in this package rather than the system temporary directory.
	scratch, err := os.MkdirTemp(".", ".runner-test-")
	if err != nil {
		t.Fatal(err)
	}
	scratch, err = filepath.Abs(scratch)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(scratch); err != nil {
			t.Error(err)
		}
	})
	f := fixture{
		root: filepath.Join(scratch, "repository with spaces"),
		cwd:  filepath.Join(scratch, "different caller directory"),
		log:  filepath.Join(scratch, "calls.jsonl"),
	}
	fakeBin := filepath.Join(scratch, "fake tools")
	for _, dir := range []string{f.root, f.cwd, fakeBin} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	_, source, _, _ := runtime.Caller(0)
	repo := filepath.Clean(filepath.Join(filepath.Dir(source), "../../.."))
	for _, name := range []string{"run.sh", "docker-run.sh"} {
		data, err := os.ReadFile(filepath.Join(repo, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(f.root, name), data, 0755); err != nil {
			t.Fatal(err)
		}
	}
	for _, tool := range []string{"go", "docker", "id"} {
		if err := os.WriteFile(filepath.Join(fakeBin, tool), helperScript(tool), 0755); err != nil {
			t.Fatal(err)
		}
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if key == "PATH" || strings.HasPrefix(key, "RUNNER_") || strings.HasPrefix(key, "FAKE_") ||
			contains(configVariables, key) {
			continue
		}
		f.env = append(f.env, entry)
	}
	f.env = append(f.env,
		"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"RUNNER_HELPER=1", "RUNNER_TEST_BINARY="+binary, "RUNNER_LOG="+f.log,
		"MICROSOFT_CLIENT_ID=test-microsoft-client",
	)
	return f
}

func (f fixture) run(t *testing.T, script string, env map[string]string, args ...string) (string, int, []invocation) {
	t.Helper()
	environment := append([]string{}, f.env...)
	for key, value := range env {
		for i := 0; i < len(environment); i++ {
			if strings.HasPrefix(environment[i], key+"=") {
				environment = append(environment[:i], environment[i+1:]...)
				i--
			}
		}
		environment = append(environment, key+"="+value)
	}
	command := exec.Command("/bin/bash", append([]string{filepath.Join(f.root, script)}, args...)...)
	command.Dir = f.cwd
	command.Env = environment
	output, err := command.CombinedOutput()
	status := 0
	if err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatal(err)
		}
		status = exitErr.ExitCode()
	}
	var calls []invocation
	if data, err := os.ReadFile(f.log); err == nil {
		scanner := bufio.NewScanner(strings.NewReader(string(data)))
		for scanner.Scan() {
			var call invocation
			if err := json.Unmarshal(scanner.Bytes(), &call); err != nil {
				t.Fatal(err)
			}
			if call.Tool == "app" || (call.Tool == "docker" && len(call.Args) > 0 && call.Args[0] == "run") {
				if call.PID != command.Process.Pid {
					t.Errorf("runner did not exec %s: PID=%d, runner PID=%d", call.Tool, call.PID, command.Process.Pid)
				}
			}
			calls = append(calls, call)
		}
		if err := scanner.Err(); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(f.log); err != nil {
			t.Fatal(err)
		}
	} else if !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(output), status, calls
}

func contains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

func options(args []string, option string) []string {
	var values []string
	for i := 0; i+1 < len(args); i++ {
		if args[i] == option {
			values = append(values, args[i+1])
		}
	}
	return values
}

func containerEnv(t *testing.T, call invocation) map[string]string {
	t.Helper()
	env := make(map[string]string)
	for _, entry := range options(call.Args, "--env") {
		key, value, found := strings.Cut(entry, "=")
		if !found {
			t.Fatalf("environment forwarded implicitly rather than explicitly: %q", entry)
		}
		if !contains(configVariables, key) || key == "DOCKER_IMAGE" {
			t.Fatalf("unexpected forwarded variable %q", key)
		}
		env[key] = value
	}
	return env
}

func assertDockerRun(t *testing.T, calls []invocation, skip bool) invocation {
	t.Helper()
	if len(calls) != 2 {
		t.Fatalf("want two Docker calls, got %#v", calls)
	}
	first := "build"
	if skip {
		first = "image"
	}
	if calls[0].Tool != "docker" || calls[0].Args[0] != first ||
		calls[1].Tool != "docker" || calls[1].Args[0] != "run" {
		t.Fatalf("unexpected call order: %#v", calls)
	}
	run := calls[1]
	for _, flag := range []string{"--rm", "--init", "--pull=never"} {
		if !contains(run.Args, flag) {
			t.Errorf("missing %s: %q", flag, run.Args)
		}
	}
	if contains(run.Args, "--detach") || contains(run.Args, "-d") {
		t.Fatal("container must not be detached")
	}
	if got := options(run.Args, "--user"); !reflect.DeepEqual(got, []string{"1001:1002"}) {
		t.Errorf("host UID/GID = %q", got)
	}
	if got := options(run.Args, "--workdir"); !reflect.DeepEqual(got, []string{"/work"}) {
		t.Errorf("working directory = %q", got)
	}
	return run
}

func TestNativeRebuildAndExec(t *testing.T) {
	f := newFixture(t)
	args := []string{"--copy-item", "ID ! ' \" $;()", "--copy-item=second", "--unknown-original"}
	for i := 0; i < 2; i++ {
		output, status, calls := f.run(t, "run.sh", map[string]string{
			"FAKE_APP_STATUS": "37", "LIST_ALL": "true", "AUTH_DIR": "private relative state",
		}, args...)
		if status != 37 || len(calls) != 2 {
			t.Fatalf("status=%d calls=%#v output=%s", status, calls, output)
		}
		if calls[0].Tool != "go" || !reflect.DeepEqual(calls[0].Args,
			[]string{"build", "-o", "bin/cloud-storage-migration", "./src"}) {
			t.Fatalf("unexpected build: %#v", calls[0])
		}
		if calls[1].Tool != "app" || !reflect.DeepEqual(calls[1].Args, args) {
			t.Fatalf("original arguments changed: %#v", calls[1])
		}
		for _, call := range calls {
			if call.CWD != f.root {
				t.Errorf("cwd=%q, want %q", call.CWD, f.root)
			}
		}
		if calls[1].Env["LIST_ALL"] != "true" || calls[1].Env["AUTH_DIR"] != "private relative state" {
			t.Errorf("native runner changed application configuration: %#v", calls[1].Env)
		}
	}
}

func TestNativeFailedBuildNeverRunsStaleBinary(t *testing.T) {
	f := newFixture(t)
	if err := os.Mkdir(filepath.Join(f.root, "bin"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.root, "bin/cloud-storage-migration"), helperScript("app"), 0755); err != nil {
		t.Fatal(err)
	}
	output, status, calls := f.run(t, "run.sh", map[string]string{"FAKE_GO_STATUS": "23"}, "--list-all")
	if status != 23 || len(calls) != 1 || calls[0].Tool != "go" || !strings.Contains(output, "no binary was run") {
		t.Fatalf("status=%d calls=%#v output=%s", status, calls, output)
	}
}

func TestNativeResolvesSymlink(t *testing.T) {
	f := newFixture(t)
	link := filepath.Join(f.cwd, "linked runner")
	if err := os.Symlink(filepath.Join(f.root, "run.sh"), link); err != nil {
		t.Fatal(err)
	}
	output, status, calls := f.run(t, filepath.Join("..", "different caller directory", "linked runner"), nil, "--help")
	if status != 0 || len(calls) != 2 || calls[0].CWD != f.root {
		t.Fatalf("status=%d calls=%#v output=%s", status, calls, output)
	}
}

func TestRunnerBuildProgressDoesNotContaminateStdout(t *testing.T) {
	for _, tt := range []struct {
		script   string
		buildVar string
		runVar   string
	}{
		{"run.sh", "FAKE_GO_STDOUT", "FAKE_APP_STDOUT"},
		{"docker-run.sh", "FAKE_BUILD_STDOUT", "FAKE_RUN_STDOUT"},
	} {
		t.Run(tt.script, func(t *testing.T) {
			f := newFixture(t)
			command := exec.Command("/bin/bash", filepath.Join(f.root, tt.script), "--list-all")
			command.Dir = f.cwd
			command.Env = append(f.env, tt.buildVar+"=build progress\n", tt.runVar+"={\"id\":\"ID\"}\n")
			var stdout, stderr bytes.Buffer
			command.Stdout, command.Stderr = &stdout, &stderr
			if err := command.Run(); err != nil {
				t.Fatalf("run failed: %v, stderr=%s", err, stderr.String())
			}
			if stdout.String() != "{\"id\":\"ID\"}\n" || stderr.String() != "build progress\n" {
				t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
			}
		})
	}
}

func TestDockerListIsSourceOnlyAndPrivate(t *testing.T) {
	f := newFixture(t)
	// Executing this file would break source-only listing.
	if err := os.WriteFile(filepath.Join(f.root, ".env"), []byte("exit 89\n"), 0600); err != nil {
		t.Fatal(err)
	}
	output, status, calls := f.run(t, "docker-run.sh", map[string]string{
		"GOOGLE_CLIENT_CREDENTIALS_FILE": "does not exist.json",
		"GOOGLE_OAUTH_PORT":              "invalid-for-copy",
		"GOOGLE_OAUTH_LISTEN_HOST":       "not-a-host",
		"COPY_ITEM_IDS":                  "invalid inherited JSON",
		"UNRELATED_HOST_SECRET":          "must-not-be-forwarded",
	}, "--list-all")
	if status != 0 {
		t.Fatalf("status=%d output=%s", status, output)
	}
	run := assertDockerRun(t, calls, false)
	if !reflect.DeepEqual(calls[0].Args, []string{"build", "--tag", "cloud-storage-migration:latest", f.root}) {
		t.Errorf("build = %q", calls[0].Args)
	}
	env := containerEnv(t, run)
	if env["LIST_ALL"] != "true" || env["COPY_ITEM_IDS"] != "[]" ||
		env["AUTH_DIR"] != "/state" || env["TEMP_COPY_DIR"] != "/work/temp-copy" ||
		env["GOOGLE_OAUTH_LISTEN_HOST"] != "0.0.0.0" {
		t.Errorf("container configuration = %#v", env)
	}
	if _, found := env["GOOGLE_CLIENT_CREDENTIALS_FILE"]; found {
		t.Fatal("listing forwarded Google credentials")
	}
	if len(options(run.Args, "--publish")) != 0 || len(options(run.Args, "--mount")) != 2 {
		t.Fatalf("listing must have two disk mounts and no publication: %q", run.Args)
	}
	for _, name := range []string{".auth", "temp-copy"} {
		info, err := os.Stat(filepath.Join(f.root, name))
		if err != nil || info.Mode().Perm() != 0700 {
			t.Fatalf("%s is not a persistent private directory: info=%v err=%v", name, info, err)
		}
	}
}

func TestDockerCopyJSONAndMountEscaping(t *testing.T) {
	f := newFixture(t)
	credentialName := "credentials, \"desktop\".json"
	if err := os.WriteFile(filepath.Join(f.root, credentialName), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	ids := []string{"ABC!123'\"$`;&()[]{}@#%^*=+?,<>|", "plain-file.txt", "unicode-😀-é"}
	args := []string{"--copy-item", ids[0], "--skip-build", "--copy-item=" + ids[1], "--copy-item", ids[2]}
	output, status, calls := f.run(t, "docker-run.sh", map[string]string{
		"LIST_ALL":                       "malformed but overridden",
		"COPY_ITEM_IDS":                  "malformed but overridden",
		"GOOGLE_CLIENT_CREDENTIALS_FILE": credentialName,
		"AUTH_DIR":                       "state, \"private\"",
		"TEMP_COPY_DIR":                  "staging with spaces",
		"GOOGLE_OAUTH_PORT":              "09876",
		"GOOGLE_OAUTH_LISTEN_HOST":       "127.0.0.1",
		"DOCKER_IMAGE":                   "local/test:custom",
	}, args...)
	if status != 0 {
		t.Fatalf("status=%d output=%s", status, output)
	}
	run := assertDockerRun(t, calls, true)
	if !reflect.DeepEqual(calls[0].Args, []string{"image", "inspect", "--", "local/test:custom"}) {
		t.Errorf("skip build must inspect the selected local tag: %q", calls[0].Args)
	}
	env := containerEnv(t, run)
	var gotIDs []string
	if err := json.Unmarshal([]byte(env["COPY_ITEM_IDS"]), &gotIDs); err != nil || !reflect.DeepEqual(gotIDs, ids) {
		t.Fatalf("JSON did not round-trip: %q parsed=%q err=%v", env["COPY_ITEM_IDS"], gotIDs, err)
	}
	if env["LIST_ALL"] != "false" || env["MICROSOFT_CLIENT_ID"] != "test-microsoft-client" ||
		env["GOOGLE_CLIENT_CREDENTIALS_FILE"] != "/run/credentials/google.json" {
		t.Errorf("copy environment = %#v", env)
	}
	if got := options(run.Args, "--publish"); !reflect.DeepEqual(got, []string{"127.0.0.1:9876:9876"}) {
		t.Errorf("callback must be host-loopback only: %q", got)
	}
	mounts := options(run.Args, "--mount")
	if len(mounts) != 3 {
		t.Fatalf("mounts = %q", mounts)
	}
	for i, want := range [][]string{
		{"type=bind", "src=" + filepath.Join(f.root, "state, \"private\""), "dst=/state"},
		{"type=bind", "src=" + filepath.Join(f.root, "staging with spaces"), "dst=/work/temp-copy"},
		{"type=bind", "src=" + filepath.Join(f.root, credentialName), "dst=/run/credentials/google.json", "readonly"},
	} {
		got, err := csv.NewReader(strings.NewReader(mounts[i])).Read()
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("mount %d = %q; parsed=%q want=%q err=%v", i, mounts[i], got, want, err)
		}
	}
	if run.Args[len(run.Args)-1] != "local/test:custom" ||
		contains(run.Args, "--copy-item") || contains(run.Args, "--list-all") {
		t.Fatal("actions must be environment-driven, never container command arguments")
	}
}

func TestDockerEnvironmentActionsAndBooleans(t *testing.T) {
	tests := []struct {
		name        string
		args        []string
		env         map[string]string
		list        string
		ids         []string
		interactive string
	}{
		{"environment list", nil, map[string]string{"LIST_ALL": "T"}, "true", []string{}, "true"},
		{"environment unicode", nil, map[string]string{
			"COPY_ITEM_IDS":    " \n [ \"a\\u0022b\", \"ABC!123\", \"\\u00e9\\ud83d\\ude00\" ]\t",
			"AUTH_INTERACTIVE": "0",
		}, "false", []string{"a\"b", "ABC!123", "é😀"}, "false"},
		{"CLI overrides inherited copy", []string{"--list-all=1"}, map[string]string{
			"COPY_ITEM_IDS": "[\"inherited\"]",
		}, "true", []string{}, "true"},
		{"false list with copy", []string{"--list-all=False", "--copy-item", "ID"}, map[string]string{
			"LIST_ALL": "true", "AUTH_INTERACTIVE": "FALSE",
		}, "false", []string{"ID"}, "false"},
		{"last boolean wins", []string{"--list-all=false", "--list-all=TRUE"}, nil, "true", []string{}, "true"},
		{"copy without Google credentials", []string{"--copy-item=ID"}, nil, "false", []string{"ID"}, "true"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			output, status, calls := f.run(t, "docker-run.sh", tt.env, tt.args...)
			if status != 0 {
				t.Fatalf("status=%d output=%s", status, output)
			}
			run := assertDockerRun(t, calls, false)
			env := containerEnv(t, run)
			var ids []string
			err := json.Unmarshal([]byte(env["COPY_ITEM_IDS"]), &ids)
			if err != nil || env["LIST_ALL"] != tt.list || !reflect.DeepEqual(ids, tt.ids) ||
				env["AUTH_INTERACTIVE"] != tt.interactive {
				t.Errorf("env=%#v parsed IDs=%q err=%v", env, ids, err)
			}
			publish := options(run.Args, "--publish")
			if (len(publish) > 0) != (tt.list == "false" && tt.interactive == "true") {
				t.Errorf("unexpected callback publication: %q", publish)
			}
		})
	}
}

func TestDockerBooleanAndActionParityWithNativeConfig(t *testing.T) {
	for _, value := range []string{"1", "t", "T", "TRUE", "true", "True", "0", "f", "F", "FALSE", "false", "False"} {
		active, err := strconv.ParseBool(value)
		if err != nil {
			t.Fatal(err)
		}
		for _, fromCLI := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/CLI=%t", value, fromCLI), func(t *testing.T) {
				f := newFixture(t)
				env := map[string]string{
					"MICROSOFT_CLIENT_ID": "test-microsoft-client",
					"AUTH_INTERACTIVE":    value,
				}
				var args []string
				if fromCLI {
					env["LIST_ALL"], env["COPY_ITEM_IDS"] = "invalid inherited bool", "invalid inherited JSON"
					args = []string{"--list-all=" + value}
					if !active {
						args = append(args, "--copy-item=ID!['\",]")
					}
				} else {
					env["LIST_ALL"] = value
					if !active {
						env["COPY_ITEM_IDS"] = `["ID!['\",]"]`
					}
				}
				if active {
					env["GOOGLE_OAUTH_PORT"] = "invalid and irrelevant for list"
					env["GOOGLE_OAUTH_LISTEN_HOST"] = "invalid and irrelevant for list"
				}
				cfg, err := config.Parse(args, func(key string) string { return env[key] })
				if err != nil {
					t.Fatalf("native configuration unexpectedly rejected case: %v", err)
				}
				output, status, calls := f.run(t, "docker-run.sh", env, args...)
				if status != 0 {
					t.Fatalf("wrapper disagrees with native parser: status=%d output=%s", status, output)
				}
				got := containerEnv(t, assertDockerRun(t, calls, false))
				var ids []string
				if err := json.Unmarshal([]byte(got["COPY_ITEM_IDS"]), &ids); err != nil {
					t.Fatal(err)
				}
				if got["LIST_ALL"] != strconv.FormatBool(cfg.Mode == config.List) ||
					got["AUTH_INTERACTIVE"] != strconv.FormatBool(cfg.InteractiveAuth) ||
					!reflect.DeepEqual(ids, append([]string{}, cfg.ItemIDs...)) {
					t.Fatalf("wrapper env=%#v differs from native config=%#v", got, cfg)
				}
			})
		}
	}
}

func TestDockerRejectsInvalidInputBeforeDocker(t *testing.T) {
	tests := []struct {
		name string
		args []string
		env  map[string]string
	}{
		{"no action", nil, nil},
		{"only skip", []string{"--skip-build"}, nil},
		{"conflict", []string{"--list-all", "--copy-item", "ID"}, nil},
		{"no active action", []string{"--list-all=false"}, map[string]string{"LIST_ALL": "true"}},
		{"unknown", []string{"--unknown"}, nil},
		{"positional", []string{"--list-all", "file.txt"}, nil},
		{"missing value", []string{"--copy-item"}, nil},
		{"empty value", []string{"--copy-item="}, nil},
		{"bad boolean", []string{"--list-all=yes"}, nil},
		{"bad skip boolean", []string{"--skip-build=true", "--list-all"}, nil},
		{"environment conflict", nil, map[string]string{"LIST_ALL": "true", "COPY_ITEM_IDS": `["ID"]`}},
		{"environment bad boolean", nil, map[string]string{"LIST_ALL": "yes"}},
		{"environment empty", nil, map[string]string{"COPY_ITEM_IDS": `[]`}},
		{"missing Microsoft client", []string{"--list-all"}, map[string]string{"MICROSOFT_CLIENT_ID": ""}},
		{"bad interactive", []string{"--list-all"}, map[string]string{"AUTH_INTERACTIVE": "yes"}},
		{"bad port", []string{"--copy-item", "ID"}, map[string]string{"GOOGLE_OAUTH_PORT": "65536"}},
		{"overflow port", []string{"--copy-item", "ID"}, map[string]string{"GOOGLE_OAUTH_PORT": "18446744073709560381"}},
		{"bad listen host", []string{"--copy-item", "ID"}, map[string]string{"GOOGLE_OAUTH_LISTEN_HOST": "example.com"}},
		{"missing configured credentials", []string{"--copy-item", "ID"}, map[string]string{"GOOGLE_CLIENT_CREDENTIALS_FILE": "missing.json"}},
		{"root state", []string{"--list-all"}, map[string]string{"AUTH_DIR": "/"}},
		{"same directories", []string{"--copy-item", "ID"}, map[string]string{"AUTH_DIR": "same", "TEMP_COPY_DIR": "same"}},
	}
	for _, raw := range []string{
		`null`, `{}`, `"ID"`, `[1]`, `[true]`, `[null]`, `[""]`, `["ID",]`,
		`["ID"] junk`, `["unterminated]`, `["\q"]`, `["\uD800"]`,
		`["\u0000"]`, `["\u0085"]`, `["a\/b"]`, `["a\\b"]`, `["a\nb"]`,
	} {
		tests = append(tests, struct {
			name string
			args []string
			env  map[string]string
		}{"invalid JSON " + raw, nil, map[string]string{"COPY_ITEM_IDS": raw}})
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			output, status, calls := f.run(t, "docker-run.sh", tt.env, tt.args...)
			if status != 2 || len(calls) != 0 || !strings.Contains(output, "Configuration error:") {
				t.Fatalf("status=%d calls=%#v output=%s", status, calls, output)
			}
		})
	}
}

func TestDockerIDValidationMatchesNative(t *testing.T) {
	for _, id := range []string{
		"", " ", "\u2003", ".", "..", "/path", "path/to/file", `path\file`,
		"https://example.com/id", "C:file", "-flag", "line\nbreak", "tab\tid", "ID\x7f", "ID\u0085",
		"ABC!123", "arbitrary ' \" $;[]{}()=,+?<>|", "plain-file.txt", "résumé😀",
	} {
		t.Run(fmt.Sprintf("%q", id), func(t *testing.T) {
			f := newFixture(t)
			output, status, calls := f.run(t, "docker-run.sh", nil, "--copy-item="+id)
			if err := config.ValidateID(id); err != nil {
				if status != 2 || len(calls) != 0 {
					t.Fatalf("native rejects ID, wrapper status=%d calls=%#v output=%s", status, calls, output)
				}
			} else {
				if status != 0 {
					t.Fatalf("native accepts ID, wrapper status=%d output=%s", status, output)
				}
				assertDockerRun(t, calls, false)
			}
		})
	}
}

func TestDockerHelpHasNoDependenciesOrSideEffects(t *testing.T) {
	f := newFixture(t)
	output, status, calls := f.run(t, "docker-run.sh", map[string]string{
		"PATH": "", "MICROSOFT_CLIENT_ID": "", "LIST_ALL": "bad", "COPY_ITEM_IDS": "bad",
	}, "--skip-build", "--help")
	if status != 0 || len(calls) != 0 || !strings.Contains(output, "--skip-build") {
		t.Fatalf("status=%d calls=%#v output=%s", status, calls, output)
	}
	for _, name := range []string{".auth", "temp-copy"} {
		if _, err := os.Stat(filepath.Join(f.root, name)); !os.IsNotExist(err) {
			t.Fatalf("help created state directory %s", name)
		}
	}
}

func TestDockerFailurePropagation(t *testing.T) {
	tests := []struct {
		name   string
		args   []string
		env    map[string]string
		status int
		calls  int
		text   string
	}{
		{"failed build", []string{"--list-all"}, map[string]string{"FAKE_BUILD_STATUS": "19"}, 19, 1, "no container was started"},
		{"missing skip image", []string{"--list-all", "--skip-build"}, map[string]string{"FAKE_INSPECT_STATUS": "1"}, 1, 1, "without --skip-build"},
		{"image disappears after inspect", []string{"--list-all", "--skip-build"}, map[string]string{"FAKE_RUN_STATUS": "125"}, 125, 2, ""},
		{"container status", []string{"--list-all", "--skip-build"}, map[string]string{"FAKE_RUN_STATUS": "37"}, 37, 2, ""},
		{"root host", []string{"--list-all"}, map[string]string{"FAKE_UID": "0"}, 1, 0, "non-root host user"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			output, status, calls := f.run(t, "docker-run.sh", tt.env, tt.args...)
			if status != tt.status || len(calls) != tt.calls || !strings.Contains(output, tt.text) {
				t.Fatalf("status=%d calls=%#v output=%s", status, calls, output)
			}
			for _, call := range calls {
				if len(call.Args) > 0 && call.Args[0] == "pull" {
					t.Fatal("wrapper must never pull an image")
				}
				if len(call.Args) > 0 && call.Args[0] == "run" && !contains(call.Args, "--pull=never") {
					t.Fatal("image inspection must not allow a missing-image pull at container startup")
				}
			}
		})
	}
}
