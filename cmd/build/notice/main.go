// Command notice regenerates THIRD-PARTY-NOTICES.txt from ClearlyDefined license
// data for the Go modules compiled into Orka binaries, the npm packages bundled
// into the UI, and the components that published image Dockerfiles download.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const outputName = "THIRD-PARTY-NOTICES.txt"

// Shipped images whose direct downloads need notices. Demo and test fixture
// images are not published, so they are not scanned.
var dockerfileGlobs = []string{"Dockerfile", "workers/*/Dockerfile", "workers/acp/images/*/Dockerfile"}

// Published images are built for these targets with CGO disabled.
var goPlatforms = []string{"linux/amd64", "linux/arm64"}

// Dev dependencies whose code is bundled into the shipped UI. Tailwind's base
// styles are compiled into the UI stylesheet from ui/src/index.css.
var bundledDevDependencies = []string{"tailwindcss"}

// Components with hand-written NOTICE.md sections because their terms are not
// open source licenses that ClearlyDefined can describe.
var handWrittenPrefixes = []string{"npm/npmjs/@anthropic-ai/"}

var (
	npmTarballURL  = regexp.MustCompile(`https://registry\.npmjs\.org/((?:@[^/"\s]+/)?[^/@"\s]+)/-/([^/"\s]+)\.tgz`)
	githubSource   = regexp.MustCompile(`https://codeload\.github\.com/([^/"\s]+)/([^/"\s]+)/tar\.gz/([0-9a-f]{40})`)
	debianPoolFile = regexp.MustCompile(`https://deb\.debian\.org/debian/pool/main/[^/"\s]+/[^/"\s]+/` +
		`([^_/"\s]+)_([^_/"\s]+)_([^_/."\s]+)\.deb`)
	trailingComma = regexp.MustCompile(`,(\s*[}\]])`)
)

// retryDelay and sleep are variables so tests can avoid real backoff.
var (
	retryDelay = func(attempt int) time.Duration { return time.Duration(attempt) * 15 * time.Second }
	sleep      = time.Sleep
)

func main() {
	api := flag.String("api", "https://api.clearlydefined.io", "ClearlyDefined API base URL")
	batch := flag.Int("batch", 25, "coordinates per notices request")
	flag.Parse()
	root, err := os.Getwd()
	if err == nil {
		err = run(root, *api, *batch)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "notice:", err)
		os.Exit(1)
	}
}

func run(root, api string, batch int) error {
	goCoordinates, err := goModules(root)
	if err != nil {
		return err
	}
	npmCoordinates, err := uiPackages(filepath.Join(root, "ui", "bun.lock"))
	if err != nil {
		return err
	}
	imageCoordinates, err := imageDownloads(root)
	if err != nil {
		return err
	}
	coordinates := dedupe(slices.Concat(goCoordinates, npmCoordinates, imageCoordinates))
	result, err := fetchNotices(&http.Client{Timeout: 3 * time.Minute}, api, coordinates, batch)
	if err != nil {
		return err
	}
	for _, missing := range result.missing() {
		fmt.Fprintln(os.Stderr, "notice: no ClearlyDefined license data for", missing)
	}
	for _, missing := range result.missingText() {
		fmt.Fprintln(os.Stderr, "notice: no ClearlyDefined license text for", missing)
	}
	fmt.Fprintf(os.Stderr, "notice: %d of %d components have license data\n", len(result.Packages), len(coordinates))
	return os.WriteFile(filepath.Join(root, outputName), render(result), 0o644)
}

// goModules lists the modules compiled into every main package under cmd and
// workers for each published platform. Test-only dependencies are excluded
// because -test is not set.
func goModules(root string) ([]string, error) {
	const format = `{{with .Module}}{{if not .Main}}` +
		`{{with .Replace}}{{.Path}} {{.Version}}{{else}}{{.Path}} {{.Version}}{{end}}` +
		`{{end}}{{end}}`
	var out []byte
	for _, platform := range goPlatforms {
		goos, goarch, _ := strings.Cut(platform, "/")
		cmd := exec.Command("go", "list", "-deps", "-f", format, "./cmd/...", "./workers/...")
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "GOOS="+goos, "GOARCH="+goarch, "CGO_ENABLED=0")
		cmd.Stderr = os.Stderr
		listed, err := cmd.Output()
		if err != nil {
			return nil, fmt.Errorf("list Go modules for %s: %w", platform, err)
		}
		out = append(out, listed...)
	}
	var coordinates []string
	for line := range strings.Lines(string(out)) {
		path, version, _ := strings.Cut(strings.TrimSpace(line), " ")
		// A replacement without a version is a local directory in this repository.
		if path == "" || version == "" {
			continue
		}
		coordinates = append(coordinates, goCoordinate(path, version))
	}
	return coordinates, nil
}

// goCoordinate maps a module path to ClearlyDefined's go/golang form, where the
// last path element is the name and the rest is an escaped namespace.
func goCoordinate(path, version string) string {
	namespace, name := "-", path
	if index := strings.LastIndex(path, "/"); index >= 0 {
		namespace, name = strings.ReplaceAll(path[:index], "/", "%2f"), path[index+1:]
	}
	return fmt.Sprintf("go/golang/%s/%s/%s", namespace, name, version)
}

func npmCoordinate(name, version string) string {
	if scope, base, scoped := strings.Cut(name, "/"); scoped {
		return fmt.Sprintf("npm/npmjs/%s/%s/%s", scope, base, version)
	}
	return fmt.Sprintf("npm/npmjs/-/%s/%s", name, version)
}

type bunPackage struct {
	name, version string
	dependencies  []string
}

// uiPackages walks bun.lock from the UI's production dependencies. Dev and
// optional dependencies are build tooling or platform binaries, not part of
// the bundle served by the controller.
func uiPackages(lockfile string) ([]string, error) {
	data, err := os.ReadFile(lockfile)
	if err != nil {
		return nil, err
	}
	var lock struct {
		Workspaces map[string]struct {
			Dependencies    map[string]string `json:"dependencies"`
			DevDependencies map[string]string `json:"devDependencies"`
		} `json:"workspaces"`
		Packages map[string][]json.RawMessage `json:"packages"`
	}
	if err := json.Unmarshal(trailingComma.ReplaceAll(data, []byte("$1")), &lock); err != nil {
		return nil, fmt.Errorf("parse %s: %w", lockfile, err)
	}
	packages := make(map[string]bunPackage, len(lock.Packages))
	for key, entry := range lock.Packages {
		pkg, err := parseBunPackage(entry)
		if err != nil {
			return nil, fmt.Errorf("parse %s package %q: %w", lockfile, key, err)
		}
		packages[key] = pkg
	}
	seen := map[string]bool{}
	var visit func(parent, dependency string) error
	visit = func(parent, dependency string) error {
		key, found := resolveBunPackage(packages, parent, dependency)
		if !found {
			return fmt.Errorf("%s: dependency %q of %q is not locked", lockfile, dependency, parent)
		}
		if seen[key] {
			return nil
		}
		seen[key] = true
		for _, next := range packages[key].dependencies {
			if err := visit(key, next); err != nil {
				return err
			}
		}
		return nil
	}
	workspace := lock.Workspaces[""]
	for dependency := range workspace.Dependencies {
		if err := visit("", dependency); err != nil {
			return nil, err
		}
	}
	for _, dependency := range bundledDevDependencies {
		if _, declared := workspace.DevDependencies[dependency]; !declared {
			return nil, fmt.Errorf("%s: bundled dev dependency %q is not declared", lockfile, dependency)
		}
		if err := visit("", dependency); err != nil {
			return nil, err
		}
	}
	var coordinates []string
	for key := range seen {
		coordinates = append(coordinates, npmCoordinate(packages[key].name, packages[key].version))
	}
	return coordinates, nil
}

// parseBunPackage reads a bun.lock entry: ["name@version", registry, metadata, integrity].
func parseBunPackage(entry []json.RawMessage) (bunPackage, error) {
	var ident string
	if len(entry) == 0 || json.Unmarshal(entry[0], &ident) != nil {
		return bunPackage{}, errors.New("missing package identifier")
	}
	at := strings.LastIndex(ident, "@")
	if at <= 0 {
		return bunPackage{}, fmt.Errorf("identifier %q has no version", ident)
	}
	pkg := bunPackage{name: ident[:at], version: ident[at+1:]}
	for _, raw := range entry[1:] {
		var metadata struct {
			Dependencies     map[string]string `json:"dependencies"`
			PeerDependencies map[string]string `json:"peerDependencies"`
			OptionalPeers    []string          `json:"optionalPeers"`
		}
		if json.Unmarshal(raw, &metadata) != nil {
			continue
		}
		for name := range metadata.Dependencies {
			pkg.dependencies = append(pkg.dependencies, name)
		}
		for name := range metadata.PeerDependencies {
			if !slices.Contains(metadata.OptionalPeers, name) {
				pkg.dependencies = append(pkg.dependencies, name)
			}
		}
	}
	return pkg, nil
}

// resolveBunPackage applies node_modules lookup: a nested "parent/dependency"
// key wins over each ancestor's, and the hoisted "dependency" key is last.
func resolveBunPackage(packages map[string]bunPackage, parent, dependency string) (string, bool) {
	var names []string
	segments := strings.Split(parent, "/")
	for index := 0; parent != "" && index < len(segments); index++ {
		if strings.HasPrefix(segments[index], "@") && index+1 < len(segments) {
			names = append(names, segments[index]+"/"+segments[index+1])
			index++
			continue
		}
		names = append(names, segments[index])
	}
	for depth := len(names); depth >= 0; depth-- {
		key := strings.Join(append(slices.Clone(names[:depth]), dependency), "/")
		if _, found := packages[key]; found {
			return key, true
		}
	}
	return "", false
}

// imageDownloads extracts the npm tarballs, GitHub source archives, and Debian
// packages that published Dockerfiles fetch directly.
func imageDownloads(root string) ([]string, error) {
	var coordinates []string
	for _, glob := range dockerfileGlobs {
		paths, err := filepath.Glob(filepath.Join(root, glob))
		if err != nil {
			return nil, err
		}
		for _, path := range paths {
			data, err := os.ReadFile(path)
			if err != nil {
				return nil, err
			}
			coordinates = append(coordinates, dockerfileCoordinates(string(data))...)
		}
	}
	return slices.DeleteFunc(coordinates, func(coordinate string) bool {
		return slices.ContainsFunc(handWrittenPrefixes, func(prefix string) bool {
			return strings.HasPrefix(coordinate, prefix)
		})
	}), nil
}

func dockerfileCoordinates(dockerfile string) []string {
	coordinates := make([]string, 0, 8)
	for _, match := range npmTarballURL.FindAllStringSubmatch(dockerfile, -1) {
		base := match[1][strings.LastIndex(match[1], "/")+1:]
		coordinates = append(coordinates, npmCoordinate(match[1], strings.TrimPrefix(match[2], base+"-")))
	}
	for _, match := range githubSource.FindAllStringSubmatch(dockerfile, -1) {
		coordinates = append(coordinates, fmt.Sprintf("git/github/%s/%s/%s", match[1], match[2], match[3]))
	}
	for _, match := range debianPoolFile.FindAllStringSubmatch(dockerfile, -1) {
		coordinates = append(coordinates, fmt.Sprintf("deb/debian/-/%s/%s_%s", match[1], match[2], match[3]))
	}
	return coordinates
}

func dedupe(coordinates []string) []string {
	sort.Strings(coordinates)
	return slices.Compact(coordinates)
}

type noticePackage struct {
	Name       string   `json:"name"`
	Version    string   `json:"version"`
	License    string   `json:"license"`
	Website    string   `json:"website"`
	Copyrights []string `json:"copyrights"`
	Text       string   `json:"text"`
}

type noticeResult struct {
	Packages     []noticePackage
	NoDefinition []string
	NoLicense    []string
}

func (r noticeResult) missing() []string {
	return dedupe(slices.Concat(r.NoDefinition, r.NoLicense))
}

// missingText lists packages for which ClearlyDefined found no license file and
// substituted the license expression for the text.
func (r noticeResult) missingText() []string {
	var names []string
	for _, pkg := range r.Packages {
		if text := strings.TrimSpace(pkg.Text); text == "" || text == pkg.License {
			names = append(names, pkg.Name+" "+pkg.Version)
		}
	}
	return names
}

// fetchNotices posts coordinates in batches. ClearlyDefined computes missing
// definitions on demand and its gateway returns 502 while it does, so server
// errors are retried; the next attempt usually hits the warmed cache.
func fetchNotices(client *http.Client, api string, coordinates []string, batch int) (noticeResult, error) {
	if batch < 1 {
		return noticeResult{}, errors.New("batch must be positive")
	}
	var (
		mu       sync.Mutex
		wg       sync.WaitGroup
		result   noticeResult
		firstErr error
		limit    = make(chan struct{}, 4)
	)
	for chunk := range slices.Chunk(coordinates, batch) {
		wg.Go(func() {
			limit <- struct{}{}
			defer func() { <-limit }()
			response, err := postNotices(client, api, chunk)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				return
			}
			result.Packages = append(result.Packages, response.Content.Packages...)
			result.NoDefinition = append(result.NoDefinition, response.Summary.Warnings.NoDefinition...)
			result.NoLicense = append(result.NoLicense, response.Summary.Warnings.NoLicense...)
		})
	}
	wg.Wait()
	if firstErr != nil {
		return noticeResult{}, firstErr
	}
	for index := range result.Packages {
		pkg := &result.Packages[index]
		if name, err := url.PathUnescape(pkg.Name); err == nil {
			pkg.Name = name
		}
		// Keep output stable across runs: ClearlyDefined does not order copyrights.
		pkg.Copyrights = dedupe(pkg.Copyrights)
		pkg.Text = strings.ReplaceAll(pkg.Text, "\r\n", "\n")
	}
	sort.Slice(result.Packages, func(i, j int) bool {
		left, right := result.Packages[i], result.Packages[j]
		if !strings.EqualFold(left.Name, right.Name) {
			return strings.ToLower(left.Name) < strings.ToLower(right.Name)
		}
		return left.Version < right.Version
	})
	return result, nil
}

type noticeResponse struct {
	Content struct {
		Packages []noticePackage `json:"packages"`
	} `json:"content"`
	Summary struct {
		Warnings struct {
			NoDefinition []string `json:"noDefinition"`
			NoLicense    []string `json:"noLicense"`
		} `json:"warnings"`
	} `json:"summary"`
}

func postNotices(client *http.Client, api string, coordinates []string) (noticeResponse, error) {
	body, err := json.Marshal(map[string]any{"coordinates": coordinates, "renderer": "json"})
	if err != nil {
		return noticeResponse{}, err
	}
	const attempts = 6
	var lastErr error
	var retryAfter time.Duration
	for attempt := 1; attempt <= attempts; attempt++ {
		if attempt > 1 {
			sleep(max(retryDelay(attempt-1), retryAfter))
		}
		response, err := client.Post(strings.TrimRight(api, "/")+"/notices", "application/json", bytes.NewReader(body))
		if err != nil {
			lastErr = err
			continue
		}
		data, err := io.ReadAll(io.LimitReader(response.Body, 64<<20))
		_ = response.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}
		if response.StatusCode >= 500 || response.StatusCode == http.StatusTooManyRequests {
			lastErr = fmt.Errorf("ClearlyDefined returned %s", response.Status)
			retryAfter = retryAfterDelay(response.Header.Get("Retry-After"), time.Now())
			continue
		}
		if response.StatusCode != http.StatusOK {
			return noticeResponse{}, fmt.Errorf("ClearlyDefined returned %s: %s", response.Status, bytes.TrimSpace(data))
		}
		var decoded noticeResponse
		if err := json.Unmarshal(data, &decoded); err != nil {
			return noticeResponse{}, fmt.Errorf("decode ClearlyDefined response: %w", err)
		}
		return decoded, nil
	}
	return noticeResponse{}, fmt.Errorf("notices request for %d coordinates failed after %d attempts: %w",
		len(coordinates), attempts, lastErr)
}

// retryAfterDelay reads a Retry-After header in either of its RFC 9110 forms,
// delay-seconds or an HTTP-date, and caps the wait at five minutes.
func retryAfterDelay(header string, now time.Time) time.Duration {
	var delay time.Duration
	if seconds, err := strconv.Atoi(header); err == nil {
		delay = time.Duration(seconds) * time.Second
	} else if date, err := http.ParseTime(header); err == nil {
		delay = date.Sub(now)
	}
	return min(max(delay, 0), 5*time.Minute)
}

const separator = "-------------------------------------------------------------------------------"

func render(result noticeResult) []byte {
	var out strings.Builder
	out.WriteString(`THIRD-PARTY SOFTWARE NOTICES

This file is generated by "make notice" from ClearlyDefined
(https://clearlydefined.io) license data. Do not edit it by hand.

It covers the Go modules compiled into Orka binaries, the npm packages bundled
into the Orka UI, and the components that published Orka image Dockerfiles
download directly. NOTICE.md carries hand-written notices for modified source
and for components distributed under non-open-source terms.
`)
	if missing := result.missing(); len(missing) > 0 {
		out.WriteString("\nClearlyDefined had no license data for these components, so their notices\n" +
			"are not included below:\n\n")
		for _, coordinate := range missing {
			out.WriteString("  " + coordinate + "\n")
		}
	}
	if missing := result.missingText(); len(missing) > 0 {
		out.WriteString("\nClearlyDefined had no license text for these components, so their entries\n" +
			"below name the license without reproducing it:\n\n")
		for _, name := range missing {
			out.WriteString("  " + name + "\n")
		}
	}
	for _, pkg := range result.Packages {
		out.WriteString("\n" + separator + "\n\n")
		fmt.Fprintf(&out, "%s %s\n", pkg.Name, pkg.Version)
		fmt.Fprintf(&out, "License: %s\n", pkg.License)
		if pkg.Website != "" {
			fmt.Fprintf(&out, "Website: %s\n", pkg.Website)
		}
		if len(pkg.Copyrights) > 0 {
			out.WriteString("\n" + strings.Join(pkg.Copyrights, "\n") + "\n")
		}
		if text := strings.TrimSpace(pkg.Text); text != "" && text != pkg.License {
			out.WriteString("\n" + text + "\n")
		}
	}
	return []byte(out.String())
}
