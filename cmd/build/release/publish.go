package main

import (
	"errors"
	"fmt"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

type annotatedTag struct {
	SHA     string    `json:"sha"`
	Object  gitObject `json:"object"`
	Message string    `json:"message"`
}

func (w *workflow) createTag(data candidateBundle, directory string) error {
	candidateHash, err := fileHash(filepath.Join(directory, candidateFile))
	if err != nil {
		return err
	}
	qualificationHash, err := fileHash(filepath.Join(directory, qualificationFile))
	if err != nil {
		return err
	}
	// Preserve the original tag-message representation for publication retries.
	message := fmt.Sprintf(`{"candidateSHA256": %q, "qualificationSHA256": %q}`, candidateHash, qualificationHash)
	existing, err := w.tagRef(data.Version)
	if err != nil {
		return err
	}
	if existing != nil {
		if existing.Object.Type != "tag" {
			return errors.New("existing release tag has no qualification binding")
		}
		var tag annotatedTag
		if err := w.api(repoAPI+"/git/tags/"+existing.Object.SHA, http.MethodGet, nil, &tag); err != nil {
			return err
		}
		if tag.Object.Type != gitCommit || tag.Object.SHA != data.CandidateSHA || strings.TrimSpace(tag.Message) != message {
			return errors.New("existing tag identifies different release artifacts")
		}
		return nil
	}
	var tag annotatedTag
	if err := w.api(repoAPI+"/git/tags", http.MethodPost, map[string]string{
		"tag": data.Version, "message": message, "object": data.CandidateSHA, "type": gitCommit,
	}, &tag); err != nil {
		return err
	}
	if !shaRE.MatchString(tag.SHA) {
		return errors.New("GitHub returned an invalid annotated tag")
	}
	return w.api(repoAPI+"/git/refs", http.MethodPost,
		map[string]string{"ref": "refs/tags/" + data.Version, "sha": tag.SHA}, nil)
}

var (
	missingManifestRE = regexp.MustCompile(`(?i)manifest unknown|manifest_unknown|: not found`)
	registryDeniedRE  = regexp.MustCompile(`(?i)unauthorized|denied|forbidden|429|401|403`)
)

func (w *workflow) registryDigest(ref string) (string, error) {
	result := w.run(commandSpec{dir: w.root, args: []string{
		"docker", "buildx", "imagetools", "inspect", ref, "--format", "{{json .Manifest}}",
	}})
	if result.err != nil {
		// Only a missing-manifest response establishes absence. Authentication,
		// transport, and rate-limit failures must stop publication.
		if !missingManifestRE.MatchString(result.stderr) || registryDeniedRE.MatchString(result.stderr) {
			return "", fmt.Errorf("could not inspect release image %s", ref)
		}
		return "", nil
	}
	var manifest struct {
		Digest string `json:"digest"`
	}
	if err := decodeJSON([]byte(result.stdout), &manifest); err != nil {
		return "", err
	}
	if !digestRE.MatchString(manifest.Digest) {
		return "", fmt.Errorf("registry returned an invalid digest for %s", ref)
	}
	return manifest.Digest, nil
}

func (w *workflow) checkImageVersions(data candidateBundle) error {
	for _, name := range imageNames {
		ref := data.Images[name]
		_, digest, _ := strings.Cut(ref, "@")
		actual, err := w.registryDigest(ref)
		if err != nil {
			return err
		}
		if actual != digest {
			return fmt.Errorf("qualified %s image is unavailable", name)
		}
		for _, tag := range []string{strings.TrimPrefix(data.Version, "v"), "sha-" + data.CandidateSHA[:7]} {
			existing, err := w.registryDigest(imageRepository(name) + ":" + tag)
			if err != nil {
				return err
			}
			if existing != "" && existing != digest {
				return fmt.Errorf("immutable %s:%s already points to different bytes", name, tag)
			}
		}
	}
	return nil
}

// Validated version components have no leading zeros. Decimal comparison avoids
// imposing an integer-size limit that the version format does not have.
func compareVersions(first, second string) int {
	a := strings.Split(strings.TrimPrefix(first, "v"), ".")
	b := strings.Split(strings.TrimPrefix(second, "v"), ".")
	for i := range a {
		if len(a[i]) < len(b[i]) {
			return -1
		}
		if len(a[i]) > len(b[i]) {
			return 1
		}
		if result := strings.Compare(a[i], b[i]); result != 0 {
			return result
		}
	}
	return 0
}

func (w *workflow) releaseAliases(data candidateBundle) ([]string, error) {
	version := strings.TrimPrefix(data.Version, "v")
	tags := []string{version, "sha-" + data.CandidateSHA[:7]}
	if strings.Contains(version, "-") {
		return tags, nil
	}
	var refs []gitRef
	if err := w.api(repoAPI+"/git/matching-refs/tags/v", http.MethodGet, nil, &refs); err != nil {
		return nil, err
	}
	latest, latestLine := true, true
	line := strings.Join(strings.Split(version, ".")[:2], ".")
	for _, ref := range refs {
		other := strings.TrimPrefix(ref.Ref, "refs/tags/")
		if !versionRE.MatchString(other) || strings.Contains(other, "-") {
			continue
		}
		if compareVersions(data.Version, other) < 0 {
			latest = false
			if strings.Join(strings.Split(strings.TrimPrefix(other, "v"), ".")[:2], ".") == line {
				latestLine = false
			}
		}
	}
	if latestLine {
		tags = append(tags, line)
	}
	if latest {
		tags = append(tags, "latest")
	}
	return tags, nil
}

func (w *workflow) promoteImages(data candidateBundle) error {
	tags, err := w.releaseAliases(data)
	if err != nil {
		return err
	}
	for _, name := range imageNames {
		ref := data.Images[name]
		_, digest, _ := strings.Cut(ref, "@")
		for _, tag := range tags {
			target := imageRepository(name) + ":" + tag
			current, err := w.registryDigest(target)
			if err != nil {
				return err
			}
			if current == digest {
				continue
			}
			if _, err := w.command("docker", "buildx", "imagetools", "create", "--tag", target, ref); err != nil {
				return err
			}
			current, err = w.registryDigest(target)
			if err != nil {
				return err
			}
			if current != digest {
				return fmt.Errorf("published %s does not match qualified bytes", target)
			}
		}
	}
	return nil
}

func (w *workflow) publishChart(data candidateBundle, directory string) (commit, indexHash string, err error) {
	pages, err := os.MkdirTemp("", "orka-release-pages-")
	if err != nil {
		return "", "", err
	}
	defer func() { _ = os.RemoveAll(pages) }()
	if err := w.checkoutPages(pages); err != nil {
		return "", "", err
	}
	charts := filepath.Join(pages, "charts")
	if err := os.MkdirAll(charts, 0o755); err != nil {
		return "", "", err
	}
	destination := filepath.Join(charts, data.Chart.File)
	if _, err := os.Stat(destination); err == nil {
		if err := verifyHash(destination, data.Chart.SHA256, "published chart has different bytes"); err != nil {
			return "", "", err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", "", err
	}
	if err := copyFile(filepath.Join(directory, data.Chart.File), destination); err != nil {
		return "", "", err
	}
	index := filepath.Join(charts, "index.yaml")
	previous, err := os.ReadFile(index)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", "", err
	}
	if _, err := w.commandRaw(pages, "", "helm", "repo", "index", charts, "--url", chartURL); err != nil {
		return "", "", err
	}
	current, err := os.ReadFile(index)
	if err != nil {
		return "", "", err
	}
	// Helm refreshes timestamps each time. Preserve bytes when entries already
	// match, while still repairing an absent or incomplete index.
	timestamps := regexp.MustCompile(`(?m)^(?:    created:|generated:).*$`)
	if len(previous) > 0 && string(timestamps.ReplaceAll(previous, nil)) == string(timestamps.ReplaceAll(current, nil)) {
		if err := os.WriteFile(index, previous, 0o600); err != nil {
			return "", "", err
		}
	}
	if err := w.commitChart(pages, data); err != nil {
		return "", "", err
	}
	commit, err = w.commandRaw(pages, "", gitCommand, "rev-parse", "HEAD")
	if err != nil {
		return "", "", err
	}
	indexHash, err = fileHash(index)
	return strings.TrimSpace(commit), indexHash, err
}

func (w *workflow) checkoutPages(directory string) error {
	for _, args := range [][]string{
		{gitCommand, "init", "--quiet"}, {gitCommand, "remote", "add", "origin", "https://github.com/" + repository + ".git"},
	} {
		if _, err := w.commandRaw(directory, "", args...); err != nil {
			return err
		}
	}
	var refs []gitRef
	if err := w.api(repoAPI+"/git/matching-refs/heads/"+pagesBranch, http.MethodGet, nil, &refs); err != nil {
		return err
	}
	if slices.ContainsFunc(refs, func(ref gitRef) bool { return ref.Ref == "refs/heads/"+pagesBranch }) {
		if _, err := w.commandRaw(directory, "", gitCommand, "fetch", "--depth=1", "origin", pagesBranch); err != nil {
			return err
		}
		_, err := w.commandRaw(directory, "", gitCommand, "checkout", "-B", pagesBranch, "FETCH_HEAD")
		return err
	}
	_, err := w.commandRaw(directory, "", gitCommand, "checkout", "--orphan", pagesBranch)
	return err
}

func (w *workflow) commitChart(directory string, data candidateBundle) error {
	_, err := w.commandRaw(directory, "", gitCommand, "add", "--", "charts/"+data.Chart.File, "charts/index.yaml")
	if err != nil {
		return err
	}
	changed, err := w.commandRaw(directory, "", gitCommand, gitDiff, "--cached", "--name-only")
	if err != nil {
		return err
	}
	if strings.TrimSpace(changed) != "" {
		if _, err := w.commandRaw(directory, "", botCommit("chore(release): publish "+data.Version+" chart")...); err != nil {
			return err
		}
		args := append(slices.Clone(gitAuth), "push", "origin", "HEAD:refs/heads/"+pagesBranch)
		_, err := w.commandRaw(directory, "", args...)
		return err
	}
	return nil
}

func (w *workflow) checkPages() error {
	var pages struct {
		BuildType string            `json:"build_type"`
		Source    map[string]string `json:"source"`
	}
	if err := w.api(repoAPI+"/pages", http.MethodGet, nil, &pages); err != nil {
		return err
	}
	source := map[string]string{policyBranch: pagesBranch, "path": "/"}
	if pages.BuildType != "legacy" || !maps.Equal(pages.Source, source) {
		return errors.New("release chart publication requires Pages to serve the gh-pages branch root")
	}
	return nil
}

func (w *workflow) buildPages(commit string) error {
	// Native-token pushes do not trigger Pages builds. Dispatch the Pages REST endpoint explicitly.
	if err := w.api(repoAPI+"/pages/builds", http.MethodPost, map[string]any{}, nil); err != nil {
		return err
	}
	deadline := w.now().Add(10 * time.Minute)
	for {
		var build struct {
			Commit string `json:"commit"`
			Status string `json:"status"`
		}
		if err := w.api(repoAPI+"/pages/builds/latest", http.MethodGet, nil, &build); err != nil {
			return err
		}
		if build.Commit == commit {
			if build.Status == "errored" {
				return errors.New("GitHub Pages build failed; retry publication")
			}
			if build.Status == "built" {
				return nil
			}
		}
		if !w.now().Before(deadline) {
			return errors.New("GitHub Pages did not serve the published chart commit in time")
		}
		w.sleep(10 * time.Second)
	}
}

func (w *workflow) verifyServedChart(data candidateBundle, indexHash string) error {
	expected := []releaseArtifact{data.Chart, {File: "index.yaml", SHA256: indexHash}}
	deadline := w.now().Add(120 * time.Second)
	for {
		matched := true
		for _, artifact := range expected {
			body, err := w.get(chartURL + "/" + artifact.File)
			if err != nil || hashBytes(body) != artifact.SHA256 {
				matched = false
			}
		}
		if matched {
			return nil
		}
		if !w.now().Before(deadline) {
			return errors.New("pages is not serving the qualified chart and index; retry publication")
		}
		w.sleep(10 * time.Second)
	}
}

type releaseMetadata struct {
	TagName         string `json:"tag_name"`
	TargetCommitish string `json:"target_commitish"`
	Name            string `json:"name"`
	Prerelease      bool   `json:"prerelease"`
	Body            string `json:"body"`
}

type releaseRecord struct {
	ID              int64         `json:"id"`
	TagName         string        `json:"tag_name"`
	TargetCommitish string        `json:"target_commitish"`
	Name            string        `json:"name"`
	Prerelease      *bool         `json:"prerelease"`
	Body            string        `json:"body"`
	Draft           *bool         `json:"draft"`
	Assets          []runArtifact `json:"assets"`
}

func metadataFor(data candidateBundle, proof qualificationProof) releaseMetadata {
	return releaseMetadata{TagName: data.Version, TargetCommitish: data.CandidateSHA, Name: data.Version,
		Prerelease: strings.Contains(data.Version, "-"), Body: fmt.Sprintf("Release candidate `%s`.\n\n"+
			"[Build and approval](https://github.com/%s/actions/runs/%s).\n"+
			"[Release qualification evidence](https://github.com/%s/actions/runs/%s).\n\n"+
			"Attached manifests bind the exact chart, images, CLI archives, and qualification evidence.",
			data.CandidateSHA, repository, data.BuildRunID, repository, proof.RunID)}
}

func (w *workflow) findRelease(metadata releaseMetadata) (releaseRecord, error) {
	releases, err := paginated[releaseRecord](w, repoAPI+"/releases?per_page=100", "")
	if err != nil {
		return releaseRecord{}, err
	}
	var match *releaseRecord
	for _, release := range releases {
		if release.TagName != metadata.TagName {
			continue
		}
		if match != nil {
			return releaseRecord{}, errors.New("ambiguous GitHub Release")
		}
		match = &release
	}
	if match == nil {
		match = &releaseRecord{}
		payload := struct {
			releaseMetadata
			Draft bool `json:"draft"`
		}{releaseMetadata: metadata, Draft: true}
		if err := w.api(repoAPI+"/releases", http.MethodPost, payload, match); err != nil {
			return releaseRecord{}, err
		}
	}
	if match.TagName != metadata.TagName || match.TargetCommitish != metadata.TargetCommitish ||
		match.Name != metadata.Name || match.Prerelease == nil || *match.Prerelease != metadata.Prerelease ||
		match.Body != metadata.Body || match.Draft == nil {
		return releaseRecord{}, errors.New("GitHub Release metadata differs from this candidate; refusing to reuse it")
	}
	return *match, nil
}

func (w *workflow) archiveRelease(data candidateBundle, directory string) error {
	var proof qualificationProof
	if err := readJSON(filepath.Join(directory, qualificationFile), &proof); err != nil {
		return err
	}
	release, err := w.findRelease(metadataFor(data, proof))
	if err != nil {
		return err
	}
	assets := []string{candidateFile, qualificationFile, acceptanceFile, data.Chart.File}
	for _, asset := range data.CLI {
		assets = append(assets, asset.File)
	}
	for _, name := range assets {
		if err := w.archiveAsset(release.ID, data.Version, filepath.Join(directory, name)); err != nil {
			return err
		}
	}
	if *release.Draft {
		aliases, err := w.releaseAliases(data)
		if err != nil {
			return err
		}
		return w.api(repoAPI+"/releases/"+strconv.FormatInt(release.ID, 10), http.MethodPatch, map[string]any{
			"draft": false, "make_latest": strconv.FormatBool(slices.Contains(aliases, "latest")),
		}, nil)
	}
	return nil
}

func (w *workflow) archiveAsset(id int64, version, path string) error {
	var current releaseRecord
	if err := w.api(repoAPI+"/releases/"+strconv.FormatInt(id, 10), http.MethodGet, nil, &current); err != nil {
		return err
	}
	name, count := filepath.Base(path), 0
	for _, asset := range current.Assets {
		if asset.Name == name {
			count++
		}
	}
	if count > 1 {
		return fmt.Errorf("ambiguous release asset %s", name)
	}
	if count == 0 {
		if _, err := w.command("gh", "release", "upload", version, path, "--repo", repository); err != nil {
			return err
		}
	}
	// Verify completed uploads too, including those from an interrupted attempt.
	temporary, err := os.MkdirTemp("", "orka-release-asset-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(temporary) }()
	if _, err := w.command("gh", "release", "download", version, "--repo", repository,
		"--pattern", name, "--dir", temporary); err != nil {
		return err
	}
	digest, err := fileHash(path)
	if err != nil {
		return err
	}
	return verifyHash(filepath.Join(temporary, name), digest,
		"release asset "+name+" has different bytes; refusing to overwrite it")
}

func (w *workflow) publish(directory string) error {
	data, err := w.verifyPublication(directory)
	if err != nil {
		return err
	}
	if err := w.checkPages(); err != nil {
		return err
	}
	if err := w.checkImageVersions(data); err != nil {
		return err
	}
	if err := w.createTag(data, directory); err != nil {
		return err
	}
	if err := w.promoteImages(data); err != nil {
		return err
	}
	commit, indexHash, err := w.publishChart(data, directory)
	if err != nil {
		return err
	}
	if err := w.buildPages(commit); err != nil {
		return err
	}
	if err := w.verifyServedChart(data, indexHash); err != nil {
		return err
	}
	if err := w.archiveRelease(data, directory); err != nil {
		return err
	}
	return w.summary(fmt.Sprintf("Published `%s` from `%s`. "+
		"The tag binds the exact candidate and qualification artifact hashes.",
		data.Version, data.CandidateSHA))
}
