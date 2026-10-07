// Package ci holds no agent code: it collects the checks on how the agent is built and published.
// It lives here, and not in a separate script, because it must run in both pipelines — the pull
// request one and the release one — with the same commands as the other tests.
//
// CYAG-11. Two defects nobody had noticed yet, because so far nine versions had shipped and all of
// them were final:
//
//  1. every tag pushed the moving `latest` reference, previews included. Whoever installs the agent
//     copies instructions from the app that say `latest`: a preview would have landed on the servers
//     of whoever was installing at that moment. With the new way of working a preview ships before
//     EVERY production release, so the defect would show up on the first change touching the agent.
//
//  2. the release check ran fewer checks than the pull request one: without `-tags testing` 13 test
//     files were left out, including the only one that exercises the command reporting whether the
//     agent is alive — the one feeding the Dockerfile HEALTHCHECK. Breaking it turned every pull
//     request red while publishing stayed green.
//
// A note on how these tests are written, because it is the point. A check on a workflow tends to
// verify something weaker than what it claims: it looks at the text of a command and not whether
// the command runs, it looks at whether a step exists and not whether its result is used by
// anyone. An adversarial review of this file found six of that kind, each proven by mutating a copy
// of the workflow and watching the suite stay green. That is where the checks on `if:`,
// `continue-on-error`, `runs-on`, `id:`, `with.tags` and `env` come from.
package ci

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type step struct {
	Name            string            `yaml:"name"`
	ID              string            `yaml:"id"`
	Uses            string            `yaml:"uses"`
	Run             string            `yaml:"run"`
	If              any               `yaml:"if"`
	ContinueOnError any               `yaml:"continue-on-error"`
	Env             map[string]string `yaml:"env"`
	With            map[string]any    `yaml:"with"`
}

type job struct {
	RunsOn          any    `yaml:"runs-on"`
	Needs           any    `yaml:"needs"`
	If              any    `yaml:"if"`
	ContinueOnError any    `yaml:"continue-on-error"`
	Steps           []step `yaml:"steps"`
}

type workflow struct {
	Concurrency struct {
		Group string `yaml:"group"`
		// Not `bool`: ci.yml puts an expression there (`${{ github.ref_type != 'tag' }}`), which is
		// a string. A bool-typed field would make reading the whole file fail — and the test would
		// die on a parse error instead of saying what is wrong with the workflow.
		CancelInProgress any    `yaml:"cancel-in-progress"`
		Queue            string `yaml:"queue"`
	} `yaml:"concurrency"`
	Jobs map[string]job `yaml:"jobs"`
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("repository root: %v", err)
	}
	return dir
}

func readFile(t *testing.T, parts ...string) []byte {
	t.Helper()
	path := filepath.Join(append([]string{repoRoot(t)}, parts...)...)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return data
}

func readWorkflow(t *testing.T, name string) workflow {
	t.Helper()
	var w workflow
	if err := yaml.Unmarshal(readFile(t, ".github", "workflows", name), &w); err != nil {
		t.Fatalf("%s is not valid YAML: %v", name, err)
	}
	return w
}

// The `run` commands of a job, in order.
func commands(j job) []string {
	var out []string
	for _, s := range j.Steps {
		if c := strings.TrimSpace(s.Run); c != "" {
			out = append(out, c)
		}
	}
	return out
}

// A "defused" step is a step that is there but may not count: a condition that skips it, or a
// `continue-on-error` that swallows its red. It is the silent way a gate stops being a gate without
// anyone removing it.
func defused(j job) []string {
	var problems []string
	if j.If != nil {
		problems = append(problems, fmt.Sprintf("the whole job has a condition `if: %v`", j.If))
	}
	if j.ContinueOnError != nil && fmt.Sprint(j.ContinueOnError) != "false" {
		problems = append(problems, fmt.Sprintf("the whole job has `continue-on-error: %v`", j.ContinueOnError))
	}
	for _, s := range j.Steps {
		name := s.Name
		if name == "" {
			name = strings.TrimSpace(s.Run + s.Uses)
		}
		if s.If != nil {
			problems = append(problems, fmt.Sprintf("step %q has a condition `if: %v`", name, s.If))
		}
		if s.ContinueOnError != nil && fmt.Sprint(s.ContinueOnError) != "false" {
			problems = append(problems, fmt.Sprintf("step %q has `continue-on-error: %v`", name, s.ContinueOnError))
		}
	}
	return problems
}

// The release check must run everything the pull request check runs, in the same order. Not just
// "the same commands": the release has one more that would make no sense on a pull request (the
// tag shape), and demanding exact equality would force weakening the comparison. What matters is
// that nothing running on pull requests is missing before publishing.
func TestReleaseCheckRunsNoLessThanPullRequestCheck(t *testing.T) {
	pullRequest := commands(readWorkflow(t, "ci.yml").Jobs["test"])
	release := commands(readWorkflow(t, "release.yml").Jobs["test"])

	if len(pullRequest) == 0 {
		t.Fatal("the `test` job of ci.yml runs no command")
	}

	// Every pull request command appears in the release, in the same relative order.
	i := 0
	var missing []string
	for _, c := range pullRequest {
		found := false
		for ; i < len(release); i++ {
			if release[i] == c {
				found = true
				i++
				break
			}
		}
		if !found {
			missing = append(missing, c)
		}
	}
	if len(missing) > 0 {
		t.Errorf("the release check does not run (or runs out of order): %v\npull request: %v\nrelease: %v",
			missing, pullRequest, release)
	}

	// Extra commands must be only those that would make no sense on a pull request. An extra
	// command not declared here is a branch nobody has evaluated.
	releaseOnly := map[string]bool{"./script/release-tag.sh --check": true}
	for _, c := range release {
		if !contains(pullRequest, c) && !releaseOnly[c] {
			t.Errorf("the release check runs %q, which the pull request check does not run and which is not declared as release-specific", c)
		}
	}

	for _, expected := range []string{"make tags-check", "make vet", "make cover", "go build ./..."} {
		if !contains(release, expected) {
			t.Errorf("the release check does not run %q", expected)
		}
		if !contains(pullRequest, expected) {
			t.Errorf("the pull request check does not run %q", expected)
		}
	}
}

// The lists of compiled test files match only if the two jobs run on the same system. With
// identical commands but `runs-on: macos-15` on the release, every linux-only test is left out and
// no command comparison notices.
func TestBothChecksRunOnTheSameSystem(t *testing.T) {
	pullRequest := readWorkflow(t, "ci.yml").Jobs["test"]
	release := readWorkflow(t, "release.yml").Jobs["test"]
	if !reflect.DeepEqual(pullRequest.RunsOn, release.RunsOn) {
		t.Errorf("different runs-on: pull request %v, release %v — the lists of compiled test files cannot match",
			pullRequest.RunsOn, release.RunsOn)
	}
}

// Neither check may be skipped or swallowed.
func TestNoCheckIsDefused(t *testing.T) {
	for _, tc := range []struct{ file, job string }{
		{"ci.yml", "test"},
		{"release.yml", "test"},
	} {
		for _, problem := range defused(readWorkflow(t, tc.file).Jobs[tc.job]) {
			t.Errorf("%s, job %q: %s", tc.file, tc.job, problem)
		}
	}
}

// The two publishing jobs must depend on the check — and the check must be able to actually fail.
// `continue-on-error: true` on the check would make it count as successful for `needs`: with red
// tests, docker and goreleaser would start anyway.
func TestPublishersWaitForTheCheck(t *testing.T) {
	w := readWorkflow(t, "release.yml")
	for _, name := range []string{"docker", "goreleaser"} {
		j, ok := w.Jobs[name]
		if !ok {
			t.Errorf("job %q no longer exists", name)
			continue
		}
		if !dependsOn(j.Needs, "test") {
			t.Errorf("job %q does not wait for the check (needs: %v)", name, j.Needs)
		}
		if j.If != nil {
			t.Errorf("job %q has a condition `if: %v`: it might not run", name, j.If)
		}
	}
}

// `needs` must name exactly `test`, not a job that happens to contain that substring.
func dependsOn(needs any, name string) bool {
	switch v := needs.(type) {
	case string:
		return v == name
	case []any:
		for _, e := range v {
			if fmt.Sprint(e) == name {
				return true
			}
		}
	}
	return false
}

// Two releases close together fought over the moving reference, and whichever finished last won.
// Three things are needed together, and each covers a different hole.
func TestOneReleaseAtATime(t *testing.T) {
	c := readWorkflow(t, "release.yml").Concurrency

	if c.Group == "" {
		t.Fatal("release.yml declares no concurrency group")
	}
	// A constant group, without expressions: ANY per-run expression (`github.ref`,
	// `github.event.ref`, `github.sha`, `github.run_id`) would give each tag its own group, and the
	// two runs would start together again. A test that only looked for the substring "github.ref"
	// would let all the others through.
	if strings.Contains(c.Group, "${{") {
		t.Errorf("group %q contains an expression: two different tags would end up in different groups and run together", c.Group)
	}
	// It must be explicitly false. An expression here would be just as dangerous: reading the file
	// would not tell whether a release can be cancelled halfway.
	if fmt.Sprint(c.CancelInProgress) != "false" {
		t.Errorf("cancel-in-progress = %v, expected false: the first release would be cancelled halfway by the second", c.CancelInProgress)
	}
	// With the default (`single`) only one run can be pending: a third tag arriving cancels the one
	// that was waiting. What is left is the tag and nothing else — neither image nor release.
	if c.Queue != "max" {
		t.Errorf("queue = %q, expected \"max\": with the default only one run can wait, and the third tag cancels the second", c.Queue)
	}
}

// --- the step computing the image references --------------------------------

func referencesStep(t *testing.T) step {
	t.Helper()
	for _, s := range readWorkflow(t, "release.yml").Jobs["docker"].Steps {
		if strings.Contains(s.Run, "GITHUB_OUTPUT") && strings.Contains(s.Run, "channel") {
			return s
		}
	}
	t.Fatal("the docker job has no step computing the image references")
	return step{}
}

func pushStep(t *testing.T) step {
	t.Helper()
	for _, s := range readWorkflow(t, "release.yml").Jobs["docker"].Steps {
		if strings.HasPrefix(s.Uses, "docker/build-push-action@") {
			return s
		}
	}
	t.Fatal("the docker job no longer has the step publishing the image")
	return step{}
}

// The missing link. The tests proved that the `meta` step computed the right references, but
// nobody checked whether those references actually reached the publishing step: replacing `tags:`
// with a fixed value including `:latest`, the suite stayed green and every preview went back to
// pushing `latest` — exactly the defect this work closes.
func TestWhatIsComputedIsWhatIsPushed(t *testing.T) {
	meta := referencesStep(t)
	push := pushStep(t)

	if meta.ID == "" {
		t.Fatal("the step computing the references has no `id:`: nobody can refer to its outputs")
	}

	expected := fmt.Sprintf("${{ steps.%s.outputs.tags }}", meta.ID)
	if got := strings.TrimSpace(fmt.Sprint(push.With["tags"])); got != expected {
		t.Errorf("the publishing step receives tags = %q, expected %q", got, expected)
	}
	if fmt.Sprint(push.With["push"]) != "true" {
		t.Errorf("push = %v: without `push: true` nothing is pushed", push.With["push"])
	}
	// The version inside the binary comes from here: if it stopped following the tag, the image
	// would claim to be a different version from the one it is published as.
	if !strings.Contains(fmt.Sprint(push.With["build-args"]), "github.ref_name") {
		t.Errorf("build-args no longer passes the tag: %v", push.With["build-args"])
	}
}

// The image name is read from the workflow, not from a constant written in the test: otherwise,
// changing registry or name in the workflow, the tests would stay green and the release would
// publish elsewhere.
func declaredImage(t *testing.T, s step) string {
	t.Helper()
	value, ok := s.Env["IMAGE"]
	if !ok || strings.TrimSpace(value) == "" {
		t.Fatal("the references step does not declare `env: IMAGE`: without it, `set -u` kills it on the real tag")
	}
	return value
}

func runStep(t *testing.T, s step, tag string) (outputs map[string]string, ok bool) {
	t.Helper()
	outputFile := filepath.Join(t.TempDir(), "output")
	if err := os.WriteFile(outputFile, nil, 0o600); err != nil {
		t.Fatalf("preparing GITHUB_OUTPUT: %v", err)
	}
	// Yes, a shell: the purpose of the test is to run EXACTLY the script that runs on GitHub, not a
	// rewrite of it. The script comes from this repository's workflow, not from outside. The value
	// that could be hostile instead — the tag name — goes through an environment variable and is
	// never interpolated into the script text: that this path holds is one of the things this test
	// verifies, with `v1.2.3 ; rm -rf /` among the cases.
	cmd := exec.Command("bash", "-c", s.Run)
	cmd.Dir = repoRoot(t) // the script invokes ./script/release-tag.sh, as on the runner
	cmd.Env = append(os.Environ(),
		"GITHUB_REF_NAME="+tag,
		"GITHUB_OUTPUT="+outputFile,
	)
	// The variables the workflow declares in its own `env` block, read from there.
	for key, value := range s.Env {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	_, err := cmd.CombinedOutput()

	data, readErr := os.ReadFile(outputFile)
	if readErr != nil {
		t.Fatalf("reading GITHUB_OUTPUT back: %v", readErr)
	}
	return parseOutput(string(data)), err == nil
}

// GITHUB_OUTPUT accepts both `key=value` and `key<<END … END` blocks.
func parseOutput(text string) map[string]string {
	out := map[string]string{}
	lines := strings.Split(text, "\n")
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if key, delimiter, found := strings.Cut(line, "<<"); found {
			var body []string
			for i++; i < len(lines) && lines[i] != delimiter; i++ {
				body = append(body, lines[i])
			}
			out[key] = strings.Join(body, "\n")
			continue
		}
		if key, value, found := strings.Cut(line, "="); found {
			out[key] = value
		}
	}
	return out
}

// Reading the image name from the workflow makes the tests honest about WHAT is pushed, but blind
// about WHERE: changing `env: IMAGE` the test would follow the new value and stay green while
// the release publishes to another registry. A second source to compare against is needed, and
// there are three in the repository that must all say the same thing: the Makefile `docker`
// target, the docker-compose example users copy, and the README.
func TestEveryoneStatesTheSameImageName(t *testing.T) {
	declared := declaredImage(t, referencesStep(t))

	sources := []struct {
		name  string
		parts []string
	}{
		{"Makefile", []string{"Makefile"}},
		{"examples/docker-compose.yml", []string{"examples", "docker-compose.yml"}},
		{"README.md", []string{"README.md"}},
	}
	for _, src := range sources {
		if !strings.Contains(string(readFile(t, src.parts...)), declared) {
			t.Errorf("%s does not mention %q: the release would publish where nobody else looks for it", src.name, declared)
		}
	}

	// The registry the workflow logs into must be the one of the image it pushes: with two different
	// values the login succeeds and the push ends up elsewhere (or fails halfway through publishing).
	registry := strings.SplitN(declared, "/", 2)[0]
	var found bool
	for _, s := range readWorkflow(t, "release.yml").Jobs["docker"].Steps {
		if strings.HasPrefix(s.Uses, "docker/login-action@") {
			found = true
			if got := fmt.Sprint(s.With["registry"]); got != registry {
				t.Errorf("the login goes to %q but the image lives on %q", got, registry)
			}
		}
	}
	if !found {
		t.Error("the docker job no longer has the registry login step")
	}
}

func TestPreviewDoesNotTakeTheLatestReference(t *testing.T) {
	meta := referencesStep(t)
	image := declaredImage(t, meta)

	cases := []struct {
		tag     string
		channel string
		moving  string
		absent  string
	}{
		{"v1.2.3", "stable", "latest", "beta"},
		{"v0.9.1", "stable", "latest", "beta"},
		{"v10.0.0", "stable", "latest", "beta"},
		{"v1.2.3-beta.1", "beta", "beta", "latest"},
		{"v1.2.3-rc.1", "beta", "beta", "latest"},
		{"v2.0.0-alpha", "beta", "beta", "latest"},
	}

	for _, tc := range cases {
		t.Run(tc.tag, func(t *testing.T) {
			outputs, ok := runStep(t, meta, tc.tag)
			if !ok {
				t.Fatalf("the step failed on a valid tag: %s", tc.tag)
			}
			if outputs["channel"] != tc.channel {
				t.Errorf("channel = %q, expected %q", outputs["channel"], tc.channel)
			}

			// No TrimSpace on individual lines: a leading space is exactly one of the ways a reference
			// becomes illegal, and cleaning it up before comparing would make the test blind to
			// exactly what it claims to check.
			lines := strings.Split(strings.TrimRight(outputs["tags"], "\n"), "\n")
			expected := []string{image + ":" + tc.tag, image + ":" + tc.moving}
			if !reflect.DeepEqual(lines, expected) {
				t.Errorf("pushed references:\n%q\nexpected:\n%q", lines, expected)
			}
			if strings.Contains(outputs["tags"], ":"+tc.absent) {
				t.Errorf("reference %q appears, which this channel must not touch", tc.absent)
			}
		})
	}
}

func TestTagThatIsNotAVersionDoesNotPublish(t *testing.T) {
	meta := referencesStep(t)

	// `+build` is excluded on purpose, for two reasons that add up: image references do not allow
	// `+`, and goreleaser looks at the semver prerelease field — in `v1.2.3+rc1` that field is empty,
	// so it would publish it as a final version while the image channel would treat it as a preview.
	cases := []string{
		"latest", "v1", "v1.2", "1.2.3", "v1.2.3.4", "v01.2.3",
		"v1.2.3+build.5", "v1.2.3 ; rm -rf /", "V1.2.3", "-v1.2.3", "",
		"v1.2.3-" + strings.Repeat("a", 130), // over the 128 characters allowed in an image tag
	}

	for _, tag := range cases {
		name := tag
		if len(name) > 24 {
			name = name[:24] + "…"
		}
		t.Run(fmt.Sprintf("%q", name), func(t *testing.T) {
			outputs, ok := runStep(t, meta, tag)
			if ok {
				t.Fatalf("the step accepted %q and would have pushed %v", tag, outputs["tags"])
			}
			if len(outputs) != 0 {
				t.Errorf("nothing must come out on a rejected tag, instead: %v", outputs)
			}
		})
	}
}

// The tag shape must be validated in the check, not only in the image job: goreleaser runs in
// parallel with docker and would not go through there. A tag the image rejects and goreleaser
// accepts produces half a release, and the half that gets done is the wrong one.
func TestTagShapeAlsoStopsGoreleaser(t *testing.T) {
	found := false
	for _, c := range commands(readWorkflow(t, "release.yml").Jobs["test"]) {
		if strings.Contains(c, "release-tag.sh") {
			found = true
		}
	}
	if !found {
		t.Error("the check does not validate the tag shape: goreleaser would publish even a tag the image rejects")
	}
}

// --- goreleaser --------------------------------------------------------------

func TestPreviewDoesNotBecomeTheLatestGitHubRelease(t *testing.T) {
	var conf struct {
		Release struct {
			Draft      bool   `yaml:"draft"`
			Disable    any    `yaml:"disable"`
			Prerelease string `yaml:"prerelease"`
		} `yaml:"release"`
	}
	if err := yaml.Unmarshal(readFile(t, ".goreleaser.yaml"), &conf); err != nil {
		t.Fatalf(".goreleaser.yaml is not valid YAML: %v", err)
	}
	// With the default, a preview becomes "Latest" on GitHub and `GET /releases/latest` returns it.
	if conf.Release.Prerelease != "auto" {
		t.Errorf("release.prerelease = %q, expected \"auto\"", conf.Release.Prerelease)
	}
	if conf.Release.Draft {
		t.Error("release.draft is true: the release would not show up at all")
	}
	if conf.Release.Disable != nil && fmt.Sprint(conf.Release.Disable) != "false" {
		t.Errorf("release.disable = %v: no release would be created", conf.Release.Disable)
	}

	// The right configuration is useless if goreleaser is then invoked in a way that does not
	// publish. The `--skip=publish` and `--snapshot` mutations went unnoticed.
	var found bool
	for _, s := range readWorkflow(t, "release.yml").Jobs["goreleaser"].Steps {
		if !strings.HasPrefix(s.Uses, "goreleaser/goreleaser-action@") {
			continue
		}
		found = true
		args := fmt.Sprint(s.With["args"])
		if !strings.Contains(args, "release") {
			t.Errorf("goreleaser is not invoked with `release`: args = %q", args)
		}
		for _, forbidden := range []string{"--skip", "--snapshot"} {
			if strings.Contains(args, forbidden) {
				t.Errorf("goreleaser arguments contain %q: it would not publish (args = %q)", forbidden, args)
			}
		}
	}
	if !found {
		t.Error("the goreleaser job no longer has the step invoking goreleaser")
	}
}

// --- the Makefile ------------------------------------------------------------

// The targets the CI runs must pass the build tags. Removing `-tags $(TEST_TAGS)` from `vet` and
// `cover` brought back the original defect — 13 test files out of the build — and no check noticed,
// `tags-check` included: that one receives the tags from outside and does not know which ones the
// other targets use.
func TestCITargetsPassTheBuildTags(t *testing.T) {
	makefile := string(readFile(t, "Makefile"))

	if !regexp.MustCompile(`(?m)^TEST_TAGS\s*:?=\s*\S`).MatchString(makefile) {
		t.Error("TEST_TAGS is not defined, or is empty: without tags the marked files are not compiled")
	}

	for _, target := range []string{"test", "vet", "cover"} {
		body := targetBody(makefile, target)
		if body == "" {
			t.Errorf("target %q no longer exists in the Makefile", target)
			continue
		}
		if !strings.Contains(body, "-tags $(TEST_TAGS)") {
			t.Errorf("target %q does not pass `-tags $(TEST_TAGS)`:\n%s", target, body)
		}
	}
}

// The command lines of a target: from the `name:` line to the first non-indented line.
func targetBody(makefile, name string) string {
	var body []string
	inside := false
	for _, line := range strings.Split(makefile, "\n") {
		if strings.HasPrefix(line, name+":") {
			inside = true
			continue
		}
		if inside {
			if line == "" || strings.HasPrefix(line, "\t") || strings.HasPrefix(line, " ") {
				body = append(body, line)
				continue
			}
			break
		}
	}
	return strings.Join(body, "\n")
}

// --- third-party actions -----------------------------------------------------

// A reference to a third-party action pointing at a movable label (`@v6`) is the same as moving
// image names: a label its owner can move to another commit whenever they want, without us
// noticing. The CI would run code nobody has read — and these runs hold the registry password, the
// token that publishes the release and the keys goreleaser uses to sign the binaries. The commit is
// the only thing that cannot be moved (CYAG-15).
//
// The check looks at ALL the files in the folder, not the two we know by name: a workflow added
// tomorrow would not appear in a hand-written list, and that is exactly the case where nobody
// rereads what was there before.
type reference struct {
	file    string
	line    int
	uses    string // `owner/action@ref`
	comment string // the comment on the same line, `# vX.Y.Z`
}

// The third-party code references contained in a workflow.
//
// It asks the same YAML parser GitHub uses, not a regular expression over the lines. The first
// version of this check looked for `uses:` in the text: `- { uses: someone/action@v1 }`,
// `uses : someone/action@v1` and `"uses": someone/action@v1` are three forms GitHub runs without
// blinking and that reading did not see. An action written in one of those forms was invisible to
// the check — not red: invisible — which is how a defense stops defending while staying green.
//
// The version comment comes from the node's `LineComment`. In a `- { uses: ... } # v1.0.0` the
// comment sits outside the braces and the parser attributes it to no node: that case turns red and
// asks to rewrite the line in the normal form, the one used across the whole ecosystem. Red and not
// green: erring on this side costs a rewrite, on the other a check that does not check.
func referencesIn(name string, data []byte) ([]reference, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("%s is not valid YAML: %w", name, err)
	}

	var out []reference
	var walk func(n *yaml.Node)
	walk = func(n *yaml.Node) {
		if n.Kind == yaml.MappingNode {
			for i := 0; i+1 < len(n.Content); i += 2 {
				key, value := n.Content[i], n.Content[i+1]
				if key.Value == "uses" && value.Kind == yaml.ScalarNode {
					// A workflow of this repository (`./.github/...`) and an image (`docker://`) are
					// not third-party code taken by name: the first is versioned in here, the second
					// has rules of its own.
					if !strings.HasPrefix(value.Value, "./") && !strings.HasPrefix(value.Value, "docker://") {
						comment := value.LineComment
						if comment == "" {
							comment = key.LineComment
						}
						out = append(out, reference{
							file:    name,
							line:    value.Line,
							uses:    value.Value,
							comment: comment,
						})
					}
				}
				walk(value)
			}
			return
		}
		for _, child := range n.Content {
			walk(child)
		}
	}
	walk(&root)
	return out, nil
}

// Parsing is the weak point of a check like this: a form it does not recognize does not turn red,
// it turns invisible, and whoever writes it has no way to notice. These are all forms GitHub runs
// without blinking; three out of five escaped the first version.
func TestReferenceParsingIsNotFooledByYAMLForm(t *testing.T) {
	cases := []struct {
		name string
		text string
	}{
		{"normal form", "steps:\n  - uses: someone/action@v1\n"},
		{"all on one line", "steps:\n  - { uses: someone/action@v1 }\n"},
		{"space before the colon", "steps:\n  - uses : someone/action@v1\n"},
		{"quoted key", "steps:\n  - \"uses\": someone/action@v1\n"},
		{"quoted value", "steps:\n  - uses: \"someone/action@v1\"\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			found, err := referencesIn("sample.yml", []byte(tc.text))
			if err != nil {
				t.Fatalf("reading the sample workflow: %v", err)
			}
			if len(found) != 1 || found[0].uses != "someone/action@v1" {
				t.Fatalf("written this way, the action escapes the check: found %+v", found)
			}
		})
	}
}

// All the third-party code references in the workflows.
//
// Fail-closed. An empty list is indistinguishable from "all good": if the folder is renamed or the
// path is wrong, every check below would pass without having looked at anything. So it demands to
// have read the two workflows we know exist and to have found at least one reference; it discovers
// the rest on its own.
func externalReferences(t *testing.T) []reference {
	t.Helper()

	dir := filepath.Join(repoRoot(t), ".github", "workflows")
	var paths []string
	for _, pattern := range []string{"*.yml", "*.yaml"} {
		found, err := filepath.Glob(filepath.Join(dir, pattern))
		if err != nil {
			t.Fatalf("looking for workflows in %s: %v", dir, err)
		}
		paths = append(paths, found...)
	}

	var names []string
	for _, p := range paths {
		names = append(names, filepath.Base(p))
	}
	for _, expected := range []string{"ci.yml", "release.yml"} {
		if !contains(names, expected) {
			t.Fatalf("I did not read %s in %s: the check is looking in the wrong place, not at a clean tree", expected, dir)
		}
	}

	var out []reference
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		found, err := referencesIn(filepath.Base(path), data)
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		out = append(out, found...)
	}

	if len(out) == 0 {
		t.Fatal("no third-party `uses:` found in the workflows: either parsing does not work, or the rule is checking nothing")
	}
	return out
}

var fullCommit = regexp.MustCompile(`^[0-9a-f]{40}$`)

// No third-party action may be referenced by version name.
func TestEveryThirdPartyActionIsPinnedToACommit(t *testing.T) {
	for _, r := range externalReferences(t) {
		_, ref, found := strings.Cut(r.uses, "@")
		if !found {
			t.Errorf("%s:%d — %q does not even say which version to use", r.file, r.line, r.uses)
			continue
		}
		if !fullCommit.MatchString(ref) {
			t.Errorf("%s:%d — %q is pinned to %q, which is a name and not a commit: whoever owns the action can move it whenever they want",
				r.file, r.line, r.uses, ref)
		}
	}
}

// A commit alone tells nobody how old what we are running is: without the version written next to
// it, updating means opening GitHub and searching by hand for where we stopped. In practice nobody
// updates anymore, and that is how pinning to a commit stops being a defense and becomes a way to
// stay stuck on a version with a hole in it.
func TestEveryPinnedActionStatesItsVersion(t *testing.T) {
	versionAlongside := regexp.MustCompile(`#\s*v\d`)
	for _, r := range externalReferences(t) {
		if !versionAlongside.MatchString(r.comment) {
			t.Errorf("%s:%d — %q does not say which version it is: a `# vX.Y.Z` comment is needed next to the commit",
				r.file, r.line, r.uses)
		}
	}
}

func contains(list []string, value string) bool {
	for _, v := range list {
		if v == value {
			return true
		}
	}
	return false
}
