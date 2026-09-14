package main

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestRegistryAbsenceIsDistinctFromAuthOrTransportFailure(t *testing.T) {
	f := newReleaseFixture(t)
	for _, entry := range []struct {
		result   commandResult
		expected string
	}{
		{result: commandResult{stdout: `{"digest":"` + testDigest + `"}`}, expected: testDigest},
		{result: commandResult{err: errors.New("absent"), stderr: "ERROR: ghcr.io/orka-agents/orka:0.2.0: not found"}},
	} {
		f.command = func(commandSpec) (commandResult, bool) { return entry.result, true }
		digest, err := f.w.registryDigest("ghcr.io/orka-agents/orka:0.2.0")
		must(t, err)
		if digest != entry.expected {
			t.Fatalf("unexpected digest %q", digest)
		}
	}
	for _, message := range []string{"unauthorized: not found", "403 forbidden", "429 too many requests", "i/o timeout"} {
		f.command = func(commandSpec) (commandResult, bool) {
			return commandResult{err: errors.New("failed"), stderr: message}, true
		}
		_, err := f.w.registryDigest("ghcr.io/orka-agents/orka:0.2.0")
		wantError(t, err, "could not inspect")
	}
}

func TestImmutableTagsAreCheckedBeforeAnyPublication(t *testing.T) {
	f := newReleaseFixture(t)
	wrong := false
	count := 0
	f.command = func(spec commandSpec) (commandResult, bool) {
		if !slices.Equal(spec.args[:4], []string{"docker", "buildx", "imagetools", "inspect"}) {
			t.Fatal("unexpected image mutation")
		}
		count++
		digest := testDigest
		if wrong && count == 2 {
			digest = "sha256:" + strings.Repeat("c", 64)
		}
		return commandResult{stdout: `{"digest":"` + digest + `"}`}, true
	}
	must(t, f.w.checkImageVersions(f.data))
	wrong, count = true, 0
	wantError(t, f.w.checkImageVersions(f.data), "already points to different bytes")
	if count != 2 {
		t.Fatalf("continued after conflicting immutable tag: %d calls", count)
	}
}

func TestOlderReleasesAndPrereleasesDoNotRollAliasesBack(t *testing.T) {
	for _, entry := range []struct {
		version             string
		published, expected []string
	}{
		{testVersion, []string{"v0.2.0", "v0.3.0"}, []string{"0.2.0", "sha-" + testSHA[:7], "0.2"}},
		{testVersion, []string{"v0.2.1"}, []string{"0.2.0", "sha-" + testSHA[:7]}},
		{"v0.2.0-rc.1", nil, []string{"0.2.0-rc.1", "sha-" + testSHA[:7]}},
		{testVersion, []string{"v0.1.99"}, []string{"0.2.0", "sha-" + testSHA[:7], "0.2", "latest"}},
	} {
		f := newReleaseFixture(t)
		f.data.Version = entry.version
		f.api = func(request apiRequest) (any, error, bool) {
			if strings.Contains(entry.version, "-") {
				t.Fatal("prerelease queried stable tags")
			}
			refs := make([]gitRef, 0, len(entry.published))
			for _, version := range entry.published {
				refs = append(refs, gitRef{Ref: "refs/tags/" + version})
			}
			return refs, nil, true
		}
		tags, err := f.w.releaseAliases(f.data)
		must(t, err)
		if !slices.Equal(tags, entry.expected) {
			t.Fatalf("aliases for %s: %v", entry.version, tags)
		}
	}
}

func TestExistingTagOnlyAllowsIdenticalCandidateAndQualification(t *testing.T) {
	f := newReleaseFixture(t)
	f.qualification(t)
	message := fmt.Sprintf(`{"candidateSHA256": %q, "qualificationSHA256": %q}`,
		hashTestFile(t, filepath.Join(f.directory, candidateFile)),
		hashTestFile(t, filepath.Join(f.directory, qualificationFile)))
	tag := annotatedTag{Object: gitObject{Type: "commit", SHA: testSHA}, Message: message}
	ref := gitRef{Ref: "refs/tags/" + testVersion, Object: gitObject{Type: "tag", SHA: strings.Repeat("d", 40)}}
	f.api = func(request apiRequest) (any, error, bool) {
		if request.method != http.MethodGet {
			t.Fatal("mutated existing tag")
		}
		if strings.Contains(request.path, "/git/matching-refs/") {
			return []gitRef{ref}, nil, true
		}
		return tag, nil, true
	}
	must(t, f.w.createTag(f.data, f.directory))
	tag.Message = "unqualified"
	wantError(t, f.w.createTag(f.data, f.directory), "different release artifacts")
	tag.Message, tag.Object.SHA = message, strings.Repeat("c", 40)
	wantError(t, f.w.createTag(f.data, f.directory), "different release artifacts")
	ref.Object.Type = "commit"
	wantError(t, f.w.createTag(f.data, f.directory), "no qualification binding")
}

func publicationFixture(t *testing.T) *releaseFixture {
	t.Helper()
	f := newReleaseFixture(t)
	f.qualification(t)
	source := qualificationPath(f.root, 456, 1)
	must(t, os.MkdirAll(filepath.Dir(source), 0o700))
	must(t, copyFile(filepath.Join(f.directory, acceptanceFile), source))
	f.api = func(request apiRequest) (any, error, bool) {
		if request.path == runPath(456) {
			return workflowRun{RunAttempt: 1}, nil, true
		}
		if request.path == runPath(123) {
			return testRun(releaseWorkflow), nil, true
		}
		if strings.Contains(request.path, "/artifacts?") {
			return map[string]any{"artifacts": []runArtifact{{Name: "release-candidate-123-1", Expired: new(false)}}}, nil, true
		}
		return nil, nil, false
	}
	f.command = func(spec commandSpec) (commandResult, bool) {
		if spec.args[0] == "bash" {
			return commandResult{}, true
		}
		if len(spec.args) >= 3 && slices.Equal(spec.args[:3], []string{"gh", "run", "download"}) {
			copyTestBundle(t, f.directory, spec.args[len(spec.args)-1])
			return commandResult{}, true
		}
		return commandResult{}, false
	}
	return f
}

func TestPublicationRequiresOriginalRunAndUnchangedQualifiedArtifacts(t *testing.T) {
	f := publicationFixture(t)
	_, err := f.w.verifyPublication(f.directory)
	must(t, err)
	f.env["GITHUB_RUN_ID"] = "999"
	_, err = f.w.verifyPublication(f.directory)
	wantError(t, err, "workflow that built")
	f.env["GITHUB_RUN_ID"] = "123"
	api := f.api
	f.api = func(request apiRequest) (any, error, bool) {
		if request.path == runPath(456) {
			return workflowRun{RunAttempt: 2}, nil, true
		}
		return api(request)
	}
	_, err = f.w.verifyPublication(f.directory)
	wantError(t, err, "qualification was rerun")
	f.api = api
	command := f.command
	f.command = func(spec commandSpec) (commandResult, bool) {
		result, handled := command(spec)
		if len(spec.args) >= 3 && slices.Equal(spec.args[:3], []string{"gh", "run", "download"}) {
			path := filepath.Join(spec.args[len(spec.args)-1], candidateFile)
			must(t, appendFile(path, "\n"))
		}
		return result, handled
	}
	_, err = f.w.verifyPublication(f.directory)
	wantError(t, err, "build artifact changed")
	f.command = command
	writeTestFile(t, filepath.Join(f.directory, acceptanceFile), "{}")
	_, err = f.w.verifyPublication(f.directory)
	wantError(t, err, "artifacts changed")
}

func TestPublicationFailureCannotCreateTagsOrPublishAssets(t *testing.T) {
	f := newReleaseFixture(t)
	wantError(t, f.w.publish(f.directory), "qualification.json")
	for _, request := range f.requests {
		if request.method != http.MethodGet {
			t.Fatalf("published without qualification: %+v", request)
		}
	}
	for _, spec := range f.commands {
		if spec.args[0] == "docker" || spec.args[0] == "helm" || slices.Contains(spec.args, "push") {
			t.Fatal("invalid evidence reached publication")
		}
	}
}

func TestPagesRequiresExactPublishedCommitAndSuccess(t *testing.T) {
	for _, outcome := range []string{"built", "errored", "stale"} {
		f := newReleaseFixture(t)
		reads := 0
		f.api = func(request apiRequest) (any, error, bool) {
			if strings.HasSuffix(request.path, "/builds") {
				if request.method != http.MethodPost {
					t.Fatal("Pages was not explicitly dispatched")
				}
				return nil, nil, true
			}
			reads++
			commit, status := testSHA, outcome
			if reads == 1 || outcome == "stale" {
				commit, status = "old", "built"
			}
			return map[string]string{"commit": commit, "status": status}, nil, true
		}
		err := f.w.buildPages(testSHA)
		if outcome == "built" {
			must(t, err)
			if reads != 2 {
				t.Fatal("did not wait for exact Pages commit")
			}
		} else {
			wantError(t, err, "")
		}
	}
}

func TestServedChartAndIndexMustMatchAfterPagesBuild(t *testing.T) {
	f := newReleaseFixture(t)
	archive := []byte(readTestFile(t, filepath.Join(f.directory, f.data.Chart.File)))
	index := []byte("index containing the qualified version")
	requests := 0
	f.w.get = func(address string) ([]byte, error) {
		requests++
		if strings.HasSuffix(address, "/index.yaml") {
			return index, nil
		}
		if requests == 1 {
			return []byte("stale chart"), nil
		}
		return archive, nil
	}
	must(t, f.w.verifyServedChart(f.data, hashBytes(index)))
	if requests != 4 {
		t.Fatalf("expected one retry, got %d requests", requests)
	}
	f.w.get = func(address string) ([]byte, error) {
		if strings.HasSuffix(address, "/index.yaml") {
			return []byte("stale index"), nil
		}
		return archive, nil
	}
	f.w.sleep = func(time.Duration) { f.clock = f.clock.Add(121 * time.Second) }
	wantError(t, f.w.verifyServedChart(f.data, hashBytes(index)), "not serving the qualified chart")
}

func TestArchiveRetriesPreserveExistingAssetsAndRejectChangedBytes(t *testing.T) {
	f := newReleaseFixture(t)
	f.qualification(t)
	record := f.record(false)
	files := map[string]string{}
	for _, asset := range record.Assets {
		files[asset.Name] = readTestFile(t, filepath.Join(f.directory, asset.Name))
	}
	f.api = func(request apiRequest) (any, error, bool) {
		if request.paginated {
			return []releaseRecord{record}, nil, true
		}
		if request.method != http.MethodGet {
			t.Fatal("mutated existing release")
		}
		return record, nil, true
	}
	f.command = func(spec commandSpec) (commandResult, bool) {
		if len(spec.args) < 3 || !slices.Equal(spec.args[:3], []string{"gh", "release", "download"}) {
			return commandResult{}, false
		}
		name := spec.args[slices.Index(spec.args, "--pattern")+1]
		dir := spec.args[slices.Index(spec.args, "--dir")+1]
		writeTestFile(t, filepath.Join(dir, name), files[name])
		return commandResult{}, true
	}
	must(t, f.w.archiveRelease(f.data, f.directory))
	files[acceptanceFile] = "changed published evidence"
	wantError(t, f.w.archiveRelease(f.data, f.directory), "refusing to overwrite")
}

func TestArchiveRejectsMismatchedMetadataBeforeUploadingOrPublishing(t *testing.T) {
	for _, version := range []string{testVersion, "v0.2.0-rc.1"} {
		for _, draft := range []bool{true, false} {
			for _, change := range []func(*releaseRecord){
				func(r *releaseRecord) { r.TargetCommitish = strings.Repeat("c", 40) },
				func(r *releaseRecord) { r.Name = "Stale title" },
				func(r *releaseRecord) { opposite := !*r.Prerelease; r.Prerelease = &opposite },
				func(r *releaseRecord) { r.Body = "Stale evidence links" },
			} {
				f := newReleaseFixture(t)
				f.qualification(t)
				f.data.Version = version
				record := f.record(draft)
				change(&record)
				f.api = func(request apiRequest) (any, error, bool) {
					if !request.paginated {
						t.Fatal("mismatched metadata caused an API call")
					}
					return []releaseRecord{record}, nil, true
				}
				wantError(t, f.w.archiveRelease(f.data, f.directory), "GitHub Release metadata differs")
				if len(f.commands) != 1 {
					t.Fatal("mismatched metadata caused upload commands")
				}
			}
		}
	}
}

func TestMatchingRCDraftPublishesAfterVerifyingExistingAssets(t *testing.T) {
	f := newReleaseFixture(t)
	f.qualification(t)
	f.data.Version = "v0.2.0-rc.1"
	record := f.record(true)
	downloads, published := 0, false
	f.api = func(request apiRequest) (any, error, bool) {
		if request.paginated {
			return []releaseRecord{record}, nil, true
		}
		if request.method == http.MethodPatch {
			if downloads != 4 || request.payload["draft"] != false || request.payload["make_latest"] != "false" {
				t.Fatalf("incorrect release publication: %d downloads, %+v", downloads, request)
			}
			published = true
		}
		return record, nil, true
	}
	f.command = func(spec commandSpec) (commandResult, bool) {
		if len(spec.args) < 3 || !slices.Equal(spec.args[:3], []string{"gh", "release", "download"}) {
			return commandResult{}, false
		}
		if spec.args[3] != f.data.Version {
			t.Fatal("downloaded wrong release")
		}
		name := spec.args[slices.Index(spec.args, "--pattern")+1]
		dir := spec.args[slices.Index(spec.args, "--dir")+1]
		must(t, copyFile(filepath.Join(f.directory, name), filepath.Join(dir, name)))
		downloads++
		return commandResult{}, true
	}
	must(t, f.w.archiveRelease(f.data, f.directory))
	if !published {
		t.Fatal("matching draft was not published")
	}
}

func TestNewReleaseUploadsAndVerifiesAssetsBeforePublishing(t *testing.T) {
	f := newReleaseFixture(t)
	f.qualification(t)
	record := f.record(true)
	record.Assets = nil
	uploaded, downloaded := 0, 0
	f.api = func(request apiRequest) (any, error, bool) {
		if request.paginated {
			return []releaseRecord{}, nil, true
		}
		if request.path == repoAPI+"/git/matching-refs/tags/v" {
			return []gitRef{}, nil, true
		}
		if request.method == http.MethodPost && request.path == repoAPI+"/releases" {
			if request.payload["draft"] != true || request.payload["tag_name"] != testVersion ||
				request.payload["target_commitish"] != testSHA {
				t.Fatalf("wrong draft metadata: %+v", request.payload)
			}
		}
		if request.method == http.MethodPatch &&
			(uploaded != 4 || downloaded != 4 || request.payload["make_latest"] != "true") {
			t.Fatalf("premature release publication: %d uploads, %d downloads", uploaded, downloaded)
		}
		return record, nil, true
	}
	f.command = func(spec commandSpec) (commandResult, bool) {
		if len(spec.args) < 3 || spec.args[0] != "gh" || spec.args[1] != "release" {
			return commandResult{}, false
		}
		switch spec.args[2] {
		case "upload":
			uploaded++
			record.Assets = append(record.Assets, runArtifact{Name: filepath.Base(spec.args[4])})
		case "download":
			downloaded++
			name := spec.args[slices.Index(spec.args, "--pattern")+1]
			dir := spec.args[slices.Index(spec.args, "--dir")+1]
			must(t, copyFile(filepath.Join(f.directory, name), filepath.Join(dir, name)))
		default:
			t.Fatalf("unexpected release command: %v", spec.args)
		}
		return commandResult{}, true
	}
	must(t, f.w.archiveRelease(f.data, f.directory))
	if uploaded != 4 || downloaded != 4 || f.requests[len(f.requests)-1].method != http.MethodPatch {
		t.Fatal("release was not fully verified and published")
	}
}
