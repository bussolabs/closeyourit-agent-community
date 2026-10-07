package pusher

// CYAG-16 — the sample the agent ships has a written contract, and these are the tests that tie it
// to the code.
//
// Before this file the sample format lived in two places nobody compared: the Go structs of this
// package and the `Normalize` of closeyourit-rails. A field added on one side and not understood by
// the other passed the checks of both and broke in production, where no test looks. The canonical
// contract lives in closeyourit-docs (`contracts/servers-sample/v1`); here lives a copy pinned to a
// commit of that repository (`contracts/servers-sample/LOCK.json`), and the tests below use it as an
// oracle:
//
//   - the copy is intact and says where it comes from (checksums + LOCK);
//   - the contract examples sit on the side they declare;
//   - the sample the Go structs actually produce conforms, and a non-conforming one is rejected;
//   - the HTTP outcomes the contract lists produce in the agent the disposition it declares.
//
// The server is tested on the SAME examples by the twin ticket on the closeyourit-rails side: that
// is why the fixtures are a shared bundle and not a local fixture.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bussolabs/closeyourit-agent/internal/entities/system"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

const (
	contractDir         = "../../contracts/servers-sample"
	contractVersion     = "v1"
	canonicalRepository = "https://github.com/bussolabs/closeyourit-docs"
)

func bundlePath(parts ...string) string {
	return filepath.Join(append([]string{contractDir, contractVersion}, parts...)...)
}

func readBundle(t *testing.T, parts ...string) []byte {
	t.Helper()
	path := bundlePath(parts...)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return data
}

type contractExample struct {
	Path       string `json:"path"`
	Definition string `json:"definition"`
	Valid      bool   `json:"valid"`
}

type contractManifest struct {
	Contract  string            `json:"contract"`
	Version   int               `json:"version"`
	Schema    string            `json:"schema"`
	Fixtures  []contractExample `json:"fixtures"`
	HTTPCases string            `json:"http_cases"`
	AuthCases string            `json:"auth_cases"`
}

func readManifest(t *testing.T) contractManifest {
	t.Helper()
	var m contractManifest
	if err := json.Unmarshal(readBundle(t, "manifest.json"), &m); err != nil {
		t.Fatalf("manifest.json is not valid JSON: %v", err)
	}
	if len(m.Fixtures) == 0 {
		t.Fatal("manifest.json lists no example")
	}
	return m
}

// The validator for a `$defs` of the vendored schema. `AssertFormat` is not a detail: without it,
// Draft 2020-12 treats `format` as an annotation and a `recorded_at` that is not a date would pass.
// The canonical verifier (scripts/verify_servers_sample_contract.py) uses a FormatChecker, so both
// sides must ask the same thing of the same examples.
func validatorFor(t *testing.T, definition string) *jsonschema.Schema {
	t.Helper()
	doc := jsonDocument(t, bundlePath("schema.json"))
	object, ok := doc.(map[string]any)
	if !ok {
		t.Fatal("schema.json is not a JSON object")
	}
	id, ok := object["$id"].(string)
	if !ok || id == "" {
		t.Fatal("schema.json has no $id: I do not know which URL to register it under")
	}

	compiler := jsonschema.NewCompiler()
	compiler.AssertFormat()
	if err := compiler.AddResource(id, doc); err != nil {
		t.Fatalf("loading the schema: %v", err)
	}
	schema, err := compiler.Compile(id + "#/$defs/" + definition)
	if err != nil {
		t.Fatalf("compiling #/$defs/%s: %v", definition, err)
	}
	return schema
}

// Documents to validate go through the library decoder (numbers as json.Number): it is the form the
// schema numeric checks expect.
func jsonDocument(t *testing.T, path string) any {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("opening %s: %v", path, err)
	}
	defer file.Close()
	doc, err := jsonschema.UnmarshalJSON(file)
	if err != nil {
		t.Fatalf("%s is not valid JSON: %v", path, err)
	}
	return doc
}

func documentFromBytes(t *testing.T, data []byte) any {
	t.Helper()
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	return doc
}

// The vendored copy is intact and complete: every file matches its checksum and no bundle file sits
// outside the list. The second check is the one that matters: without it, a fixture could be added
// in here — not present in the canonical one — and nobody would notice.
func TestVendoredBundleMatchesItsChecksums(t *testing.T) {
	listed := map[string]string{}
	for _, line := range strings.Split(string(readBundle(t, "SHA256SUMS")), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.SplitN(line, "  ", 2)
		if len(fields) != 2 {
			t.Fatalf("unreadable SHA256SUMS line: %q", line)
		}
		listed[filepath.Clean(fields[1])] = fields[0]
	}
	if len(listed) == 0 {
		t.Fatal("SHA256SUMS is empty")
	}

	for relative, expected := range listed {
		content, err := os.ReadFile(bundlePath(relative))
		if err != nil {
			t.Errorf("SHA256SUMS lists %s, which is missing: %v", relative, err)
			continue
		}
		if got := hex.EncodeToString(sha256Sum(content)); got != expected {
			t.Errorf("%s was modified in the copy: checksum %s, expected %s", relative, got, expected)
		}
	}

	root := bundlePath()
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || entry.Name() == "SHA256SUMS" {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if _, ok := listed[relative]; !ok {
			t.Errorf("%s is in the copy but not in SHA256SUMS: the bundle is no longer the canonical one", relative)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking the bundle: %v", err)
	}
}

func sha256Sum(data []byte) []byte {
	sum := sha256.Sum256(data)
	return sum[:]
}

// The LOCK says which closeyourit-docs commit the copy comes from. A null or empty `commit` is the
// "patched ahead, provenance not pinned" state: transient by definition, and not allowed here — a
// copy without provenance is not a shared contract, it is a local fixture under another name.
func TestLockStatesWhichCanonicalCommitTheCopyComesFrom(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(contractDir, "LOCK.json"))
	if err != nil {
		t.Fatalf("reading the LOCK: %v", err)
	}
	var lock struct {
		Repository string  `json:"repository"`
		Commit     *string `json:"commit"`
		Contract   string  `json:"contract"`
		Sha256sums string  `json:"sha256sums"`
	}
	if err := json.Unmarshal(data, &lock); err != nil {
		t.Fatalf("LOCK.json is not valid JSON: %v", err)
	}

	if lock.Repository != canonicalRepository {
		t.Errorf("repository = %q, expected %q", lock.Repository, canonicalRepository)
	}
	if expected := "servers-sample/" + contractVersion; lock.Contract != expected {
		t.Errorf("contract = %q, expected %q", lock.Contract, expected)
	}
	if lock.Commit == nil || strings.TrimSpace(*lock.Commit) == "" {
		t.Error("commit missing: the copy is not pinned to a closeyourit-docs commit")
	}
	if expected := hex.EncodeToString(sha256Sum(readBundle(t, "SHA256SUMS"))); lock.Sha256sums != expected {
		t.Errorf("sha256sums = %q, but the copy's SHA256SUMS hashes to %q", lock.Sha256sums, expected)
	}
}

// Every contract example sits on the side it declares. It is the check that makes the others
// useful: if the schema stopped biting, the "invalid" examples would pass and it would show here.
func TestEveryContractExampleSitsOnTheSideItDeclares(t *testing.T) {
	manifest := readManifest(t)
	validators := map[string]*jsonschema.Schema{}

	for _, example := range manifest.Fixtures {
		t.Run(example.Path, func(t *testing.T) {
			validator, ready := validators[example.Definition]
			if !ready {
				validator = validatorFor(t, example.Definition)
				validators[example.Definition] = validator
			}
			err := validator.Validate(jsonDocument(t, bundlePath(example.Path)))
			switch {
			case example.Valid && err != nil:
				t.Errorf("example declared valid but rejected: %v", err)
			case !example.Valid && err == nil:
				t.Error("example declared invalid but accepted by the schema")
			}
		})
	}
}

// An example added to the copy and not listed in the manifest would be tested by nobody.
func TestNoContractExampleIsLeftOutOfTheManifest(t *testing.T) {
	listed := map[string]bool{}
	for _, example := range readManifest(t).Fixtures {
		listed[filepath.Clean(example.Path)] = true
	}

	for _, dir := range []string{"valid", "invalid"} {
		entries, err := os.ReadDir(bundlePath("fixtures", dir))
		if err != nil {
			t.Fatalf("reading fixtures/%s: %v", dir, err)
		}
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			relative := filepath.Join("fixtures", dir, entry.Name())
			if !listed[relative] {
				t.Errorf("%s is not in the manifest: no test looks at it", relative)
			}
		}
	}
}

// The sample the agent ACTUALLY produces — marshaled Go structs, not a hand-written file — honors
// the contract. It is the missing link: until now the golden proved that the JSON did not change,
// not that it was what the server expects.
func TestAgentSampleHonorsTheContract(t *testing.T) {
	validator := validatorFor(t, "sample")

	cases := map[string]*Payload{
		"complete":          sharedGoldenPayload(),
		"minimal":           minimalConformingPayload(),
		"nothing to update": payloadWithoutPendingUpdates(),
	}
	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			produced, err := json.Marshal(payload)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if err := validator.Validate(documentFromBytes(t, produced)); err != nil {
				t.Fatalf("the produced sample does not honor the servers-sample/%s contract: %v", contractVersion, err)
			}
		})
	}
}

// Ticket scenario: a non-conforming sample is rejected by the tests, not by the server in
// production. Each case breaks ONE contract rule on the complete sample — if the schema stopped
// guarding it, this test would turn red while the one above would stay green.
func TestNonConformingSampleIsRejectedByTheTests(t *testing.T) {
	validator := validatorFor(t, "sample")

	breakages := map[string]func(map[string]any){
		"empty fingerprint": func(sample map[string]any) {
			sample["fingerprint"] = ""
		},
		"missing fingerprint": func(sample map[string]any) {
			delete(sample, "fingerprint")
		},
		"version with v prefix": func(sample map[string]any) {
			sample["agent_version"] = "v0.9.3"
		},
		"instant is not a date": func(sample map[string]any) {
			sample["recorded_at"] = "now"
		},
		"host without update counters": func(sample map[string]any) {
			delete(sample["host"].(map[string]any), "updates_available")
		},
		"data without the container key": func(sample map[string]any) {
			delete(sample["data"].(map[string]any), "container")
		},
		"container health outside the vocabulary": func(sample map[string]any) {
			containers := sample["data"].(map[string]any)["container"].([]any)
			containers[0].(map[string]any)["h"] = json.Number("9")
		},
		"unknown database role": func(sample map[string]any) {
			sample["database"].(map[string]any)["role"] = "arbiter"
		},
	}

	for name, breakIt := range breakages {
		t.Run(name, func(t *testing.T) {
			produced, err := json.Marshal(sharedGoldenPayload())
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			sample, ok := documentFromBytes(t, produced).(map[string]any)
			if !ok {
				t.Fatal("the produced sample is not a JSON object")
			}
			breakIt(sample)
			if err := validator.Validate(sample); err == nil {
				t.Error("non-conforming sample accepted by the tests")
			}
		})
	}
}

type contractOutcome struct {
	ID           string `json:"id"`
	Status       *int   `json:"status"`
	Code         string `json:"code"`
	AgentOutcome string `json:"agent_outcome"`
	Retry        bool   `json:"retry"`
}

// The HTTP outcomes the contract lists produce in the agent the disposition the contract declares.
// The run goes through `send` and not `Push` because the disposition is what matters here, not the
// wait between attempts (which has its own tests): with `Push` this test would take some twenty
// seconds of backoff to say the same thing.
func TestContractHTTPOutcomesDriveTheAgent(t *testing.T) {
	var cases []contractOutcome
	if err := json.Unmarshal(readBundle(t, "fixtures", "http", "cases.json"), &cases); err != nil {
		t.Fatalf("cases.json is not valid JSON: %v", err)
	}
	if len(cases) == 0 {
		t.Fatal("cases.json lists no outcome")
	}

	for _, tc := range cases {
		t.Run(tc.ID, func(t *testing.T) {
			if consistent := tc.Retry == (tc.AgentOutcome == "retry"); !consistent {
				t.Fatalf("the contract contradicts itself: agent_outcome=%q with retry=%v", tc.AgentOutcome, tc.Retry)
			}

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tc.Status == nil {
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(*tc.Status)
				if tc.Code != "" {
					fmt.Fprintf(w, `{"error":{"code":%q,"message":"contract outcome"}}`, tc.Code)
				}
			}))
			if tc.Status == nil {
				// The "network" outcome: nobody answers at that address.
				server.Close()
			} else {
				defer server.Close()
			}

			err := New(server.URL, "cyi_s_test").send(minimalConformingPayload())

			switch tc.AgentOutcome {
			case "ok":
				if err != nil {
					t.Fatalf("expected ok, got %v", err)
				}
			case "revoked":
				if !errors.Is(err, ErrRevoked) {
					t.Fatalf("expected ErrRevoked (long idle, no more attempts), got %v", err)
				}
			case "retry":
				if err == nil {
					t.Fatal("expected an error to retry, the push was considered successful")
				}
				if errors.Is(err, ErrRevoked) {
					t.Fatal("an outcome to retry was mistaken for a revocation: the agent would go into long idle")
				}
			default:
				t.Fatalf("unknown agent_outcome: %q", tc.AgentOutcome)
			}
		})
	}
}

// The contract response is also the one the agent takes the per-machine credential from: the
// response fixtures count as a real body, not as documentation.
func TestAgentReadsContractResponsesAsTheContractDescribesThem(t *testing.T) {
	cases := map[string]string{
		"response_accepted.json":                 "",
		"response_accepted_with_host_token.json": "",
	}
	for name := range cases {
		var response struct {
			Data struct {
				HostToken string `json:"host_token"`
			} `json:"data"`
		}
		body := readBundle(t, "fixtures", "valid", name)
		if err := json.Unmarshal(body, &response); err != nil {
			t.Fatalf("%s is not valid JSON: %v", name, err)
		}
		cases[name] = response.Data.HostToken
	}
	if cases["response_accepted_with_host_token.json"] == "" {
		t.Fatal("the first-enrollment fixture carries no host_token")
	}

	for name, expected := range cases {
		t.Run(name, func(t *testing.T) {
			body := readBundle(t, "fixtures", "valid", name)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusAccepted)
				_, _ = w.Write(body)
			}))
			defer server.Close()

			p := New(server.URL, "cyi_s_enrollment")
			if err := p.send(minimalConformingPayload()); err != nil {
				t.Fatalf("send: %v", err)
			}
			if got := p.TakeHostToken(); got != expected {
				t.Fatalf("host token = %q, expected %q", got, expected)
			}
		})
	}
}

// The poorest sample the agent can produce while staying conforming: no optional block, collectors
// at zero, Docker not reachable (`container: null`). It is the case a freshly installed host sends
// on its first tick, and the one where an extra `omitempty` would show.
func minimalConformingPayload() *Payload {
	return &Payload{
		Fingerprint:  strings.Repeat("ab", 24),
		AgentVersion: "0.9.3",
		RecordedAt:   time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC),
		Host: HostInfo{
			Hostname:                 "freshly-installed-host",
			UpdatesAvailable:         -1,
			SecurityUpdatesAvailable: -1,
		},
		Data: &system.CombinedData{Info: system.Info{AgentVersion: "0.9.3"}},
	}
}

// The up-to-date host: no reboot required, zero updates, zero security updates. It is by far the
// most common case in a well-kept fleet, and the one where an extra `omitempty` would drop from the
// sample exactly the counters the contract always wants present — the server could no longer tell
// "zero" from "I do not know".
func payloadWithoutPendingUpdates() *Payload {
	payload := minimalConformingPayload()
	payload.Host.RebootRequired = false
	payload.Host.UpdatesAvailable = 0
	payload.Host.SecurityUpdatesAvailable = 0
	return payload
}
