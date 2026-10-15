package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

const developmentVersion = "0.0.0-dev"

var developmentDockerfiles = map[string]string{
	controllerImage:      "Dockerfile",
	aiWorkerImage:        "workers/ai/Dockerfile",
	generalWorkerImage:   "workers/general/Dockerfile",
	harnessWrapperImage:  "workers/harness/Dockerfile",
	codexRuntimeImage:    "workers/acp/images/codex/Dockerfile",
	claudeRuntimeImage:   "workers/acp/images/claude/Dockerfile",
	copilotRuntimeImage:  "workers/acp/images/copilot/Dockerfile",
	opencodeRuntimeImage: "workers/acp/images/opencode/Dockerfile",
	publisherImage:       "workers/publisher/Dockerfile",
}

type developmentImage struct {
	Name       string `json:"name"`
	Repository string `json:"repository"`
	Dockerfile string `json:"dockerfile"`
}

func (w *workflow) developmentMatrix() error {
	matrix := struct {
		Include []developmentImage `json:"include"`
	}{Include: make([]developmentImage, 0, len(imageNames))}
	for _, name := range imageNames {
		dockerfile, found := developmentDockerfiles[name]
		if !found {
			return fmt.Errorf("no development Dockerfile for %s", name)
		}
		matrix.Include = append(matrix.Include, developmentImage{name, imageRepository(name), dockerfile})
	}
	return json.NewEncoder(w.stdout).Encode(matrix)
}

func (w *workflow) publishDevelopment(directory, sha string) error {
	if !shaRE.MatchString(sha) || w.env("GITHUB_REPOSITORY") != repository ||
		w.env("GITHUB_EVENT_NAME") != "push" || w.env("GITHUB_REF") != "refs/heads/main" ||
		w.env("GITHUB_SHA") != sha ||
		w.env("GITHUB_WORKFLOW_REF") != repository+"/.github/workflows/development-images.yml@refs/heads/main" {
		return errors.New("development publication requires the exact main-push workflow and commit")
	}
	if err := w.checkRunIdentity(); err != nil {
		return err
	}
	head, err := w.command(gitCommand, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	if head != sha {
		return errors.New("development publication checkout does not match the build commit")
	}
	status, err := w.command(gitCommand, "status", "--porcelain")
	if err != nil {
		return err
	}
	if status != "" {
		return errors.New("development publication requires an unchanged checkout")
	}

	images := make(map[string]string, len(imageNames))
	digests := make(map[string]string, len(imageNames))
	for _, name := range imageNames {
		data, err := os.ReadFile(filepath.Join(directory, "digest-"+name+".txt"))
		if err != nil {
			return fmt.Errorf("missing development digest for %s", name)
		}
		digest := strings.TrimSpace(string(data))
		if !digestRE.MatchString(digest) {
			return fmt.Errorf("invalid development digest for %s", name)
		}
		digests[name] = digest
		images[name] = imageRepository(name) + "@" + digest
	}

	// Check every uploaded image and immutable tag before changing any tags.
	shaTag := "sha-" + sha
	for _, name := range imageNames {
		digest, err := w.registryDigest(images[name])
		if err != nil {
			return err
		}
		if digest != digests[name] {
			return fmt.Errorf("development image bytes do not match %s", name)
		}
		existing, err := w.registryDigest(imageRepository(name) + ":" + shaTag)
		if err != nil {
			return err
		}
		if existing != "" && existing != digest {
			return fmt.Errorf("commit image tag already identifies different bytes for %s", name)
		}
	}
	for _, name := range imageNames {
		if err := w.tagDevelopmentImage(images[name], shaTag, digests[name]); err != nil {
			return err
		}
	}

	manifest := struct {
		SourceSHA string            `json:"sourceSHA"`
		Images    map[string]string `json:"images"`
	}{sha, images}
	if err := writeJSON(filepath.Join(directory, "images.json"), manifest); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(directory, "values.json"), developmentValues(digests)); err != nil {
		return err
	}

	// Workflow concurrency serializes publication. This guard also prevents an
	// older queued run from replacing the newest main build's rolling aliases.
	var current gitRef
	if err := w.api(repoAPI+"/git/ref/heads/main", http.MethodGet, nil, &current); err != nil {
		return err
	}
	if !shaRE.MatchString(current.Object.SHA) {
		return errors.New("invalid main branch identity during development publication")
	}
	if current.Object.SHA != sha {
		_, err := fmt.Fprintln(w.stdout, "Main has advanced; keeping commit images without moving development aliases.")
		return err
	}
	for _, name := range imageNames {
		if err := w.tagDevelopmentImage(images[name], developmentVersion, digests[name]); err != nil {
			return err
		}
	}
	return nil
}

func (w *workflow) tagDevelopmentImage(ref, tag, digest string) error {
	repository, _, _ := strings.Cut(ref, "@")
	target := repository + ":" + tag
	current, err := w.registryDigest(target)
	if err != nil {
		return err
	}
	if current == digest {
		return nil
	}
	if _, err := w.command("docker", "buildx", "imagetools", "create", "--tag", target, ref); err != nil {
		return err
	}
	current, err = w.registryDigest(target)
	if err != nil {
		return err
	}
	if current != digest {
		return fmt.Errorf("development image tag does not match published bytes for %s", target)
	}
	return nil
}

type developmentImageValues struct {
	Repository string `json:"repository"`
	Tag        string `json:"tag"`
	Digest     string `json:"digest"`
	PullPolicy string `json:"pullPolicy"`
}

type developmentComponentValues struct {
	Image developmentImageValues `json:"image"`
}

func developmentValues(digests map[string]string) map[string]any {
	image := func(name string) developmentImageValues {
		return developmentImageValues{Repository: imageRepository(name), Digest: digests[name], PullPolicy: "IfNotPresent"}
	}
	runtimes := make(map[string]string, len(versionedRuntimeProviders))
	for _, provider := range versionedRuntimeProviders {
		name := "acp-" + provider + "-runtime"
		runtimes[provider+"Image"] = imageRepository(name) + "@" + digests[name]
	}
	controller := struct {
		Image      developmentImageValues `json:"image"`
		ACPRuntime map[string]string      `json:"acpRuntime"`
	}{image(controllerImage), runtimes}
	return map[string]any{
		"controller": controller,
		"publisher":  developmentComponentValues{image(publisherImage)},
		"harnessV1":  developmentComponentValues{image(harnessWrapperImage)},
		"workers": map[string]developmentComponentValues{
			"ai": {image(aiWorkerImage)}, "general": {image(generalWorkerImage)},
		},
	}
}
