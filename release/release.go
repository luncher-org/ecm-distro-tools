package release

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io/ioutil"
	"net/http"
	"regexp"
	"strings"
	"text/template"
	"unicode"

	"github.com/google/go-github/v39/github"
	"github.com/rancher/ecm-distro-tools/repository"
	"github.com/sirupsen/logrus"
	"golang.org/x/mod/modfile"
	"golang.org/x/mod/semver"
)

func majMin(v string) (string, error) {
	majMin := semver.MajorMinor(v)
	if majMin == "" {
		return "", errors.New("version is not valid")
	}
	return majMin, nil
}

func trimPeriods(v string) string {
	return strings.Replace(v, ".", "", -1)
}

// capitalize returns a new string whose first letter is capitalized.
func capitalize(s string) string {
	if runes := []rune(s); len(runes) > 0 {
		for i, r := range runes {
			if unicode.IsLetter(r) {
				runes[i] = unicode.ToUpper(r)
				s = string(runes)
				break
			}
		}
	}
	return s
}

// GenReleaseNotes genereates release notes based on the given milestone,
// previous milestone, and repository.
func GenReleaseNotes(ctx context.Context, repo, milestone, prevMilestone string, client *github.Client) (*bytes.Buffer, error) {
	const templateName = "release-notes"

	funcMap := template.FuncMap{
		"majMin":      majMin,
		"trimPeriods": trimPeriods,
		"split":       strings.Split,
		"capitalize":  capitalize,
	}

	tmpl := template.New(templateName).Funcs(funcMap)
	tmpl = template.Must(tmpl.Parse(changelogTemplate))
	tmpl = template.Must(tmpl.Parse(rke2ReleaseNoteTemplate))
	tmpl = template.Must(tmpl.Parse(k3sReleaseNoteTemplate))

	content, err := repository.RetrieveChangeLogContents(ctx, client, repo, prevMilestone, milestone)
	if err != nil {
		return nil, err
	}

	// account for processing against an rc
	milestoneNoRC := milestone
	idx := strings.Index(milestone, "-rc")
	if idx != -1 {
		tmpMilestone := []rune(milestone)
		tmpMilestone = append(tmpMilestone[0:idx], tmpMilestone[idx+4:]...)
		milestoneNoRC = string(tmpMilestone)
	}

	k8sVersion := strings.Split(milestoneNoRC, "+")[0]
	markdownVersion := strings.Replace(k8sVersion, ".", "", -1)
	tmp := strings.Split(strings.Replace(k8sVersion, "v", "", -1), ".")
	majorMinor := tmp[0] + "." + tmp[1]
	changeLogSince := strings.Replace(strings.Split(prevMilestone, "+")[0], ".", "", -1)
	sqliteVersionK3S := goModLibVersion("go-sqlite3", repo, milestone)
	sqliteVersionBinding := sqliteVersionBinding(sqliteVersionK3S)

	buf := bytes.NewBuffer(nil)

	if err := tmpl.ExecuteTemplate(buf, repo, map[string]interface{}{
		"milestone":                   milestoneNoRC,
		"prevMilestone":               prevMilestone,
		"changeLogSince":              changeLogSince,
		"content":                     content,
		"k8sVersion":                  k8sVersion,
		"changeLogVersion":            markdownVersion,
		"majorMinor":                  majorMinor,
		"EtcdVersionRKE2":             buildScriptVersion("ETCD_VERSION", repo, milestone),
		"EtcdVersionK3S":              goModLibVersion("etcd/api/v3", repo, milestone),
		"ContainerdVersionK3S":        buildScriptVersion("VERSION_CONTAINERD", repo, milestone),
		"ContainerdVersionGoMod":      goModLibVersion("containerd/containerd", repo, milestone),
		"ContainerdVersionRKE2":       dockerfileVersion("hardened-containerd", repo, milestone),
		"RuncVersionGoMod":            goModLibVersion("runc", repo, milestone),
		"RuncVersionBuildScript":      buildScriptVersion("VERSION_RUNC", repo, milestone),
		"RuncVersionRKE2":             dockerfileVersion("hardened-runc", repo, milestone),
		"CNIPluginsVersion":           imageTagVersion("cni-plugins", repo, milestone),
		"MetricsServerVersion":        imageTagVersion("metrics-server", repo, milestone),
		"TraefikVersion":              imageTagVersion("traefik", repo, milestone),
		"CoreDNSVersion":              imageTagVersion("coredns", repo, milestone),
		"IngressNginxVersion":         dockerfileVersion("rke2-ingress-nginx", repo, milestone),
		"HelmControllerVersion":       goModLibVersion("helm-controller", repo, milestone),
		"FlannelVersionRKE2":          imageTagVersion("flannel", repo, milestone),
		"FlannelVersionK3S":           goModLibVersion("flannel", repo, milestone),
		"CalicoVersion":               imageTagVersion("calico-node", repo, milestone),
		"CanalCalicoVersion":          imageTagVersion("hardened-calico", repo, milestone),
		"CiliumVersion":               imageTagVersion("cilium-cilium", repo, milestone),
		"MultusVersion":               imageTagVersion("multus-cni", repo, milestone),
		"KineVersion":                 goModLibVersion("kine", repo, milestone),
		"SQLiteVersion":               sqliteVersionBinding,
		"SQLiteVersionReplaced":       strings.ReplaceAll(sqliteVersionBinding, ".", "_"),
		"LocalPathProvisionerVersion": imageTagVersion("local-path-provisioner", repo, milestone),
	}); err != nil {
		return nil, err
	}

	return buf, nil
}

// CheckUpstreamRelease takes the given org, repo, and tags and checks
// for the tags' existence.
func CheckUpstreamRelease(ctx context.Context, client *github.Client, org, repo string, tags []string) (map[string]bool, error) {
	releases := make(map[string]bool, len(tags))

	for _, tag := range tags {
		_, _, err := client.Repositories.GetReleaseByTag(ctx, org, repo, tag)
		if err != nil {
			switch err := err.(type) {
			case *github.ErrorResponse:
				if err.Response.StatusCode != http.StatusNotFound {
					return nil, err
				}
				releases[tag] = false
				continue
			default:
				return nil, err
			}
		}

		releases[tag] = true
	}

	return releases, nil
}

// VerifyAssets checks the number of assets for the
// given release and indicates if the expected number has
// been met.
func VerifyAssets(ctx context.Context, client *github.Client, repo string, tags []string) (map[string]bool, error) {
	if len(tags) == 0 {
		return nil, errors.New("no tags provided")
	}

	org, err := repository.OrgFromRepo(repo)
	if err != nil {
		return nil, err
	}

	releases := make(map[string]bool, len(tags))

	const (
		rke2Assets    = 50
		k3sAssets     = 18
		rke2Packaging = 23
	)

	for _, tag := range tags {
		if tag == "" {
			continue
		}

		release, _, err := client.Repositories.GetReleaseByTag(ctx, org, repo, tag)
		if err != nil {
			switch err := err.(type) {
			case *github.ErrorResponse:
				if err.Response.StatusCode != http.StatusNotFound {
					return nil, err
				}
				releases[tag] = false
				continue
			default:
				return nil, err
			}
		}

		if repo == "rke2" && len(release.Assets) == rke2Assets {
			releases[tag] = true
		}

		if repo == "k3s" && len(release.Assets) == k3sAssets {
			releases[tag] = true
		}

		if repo == "rke2-packing" && len(release.Assets) == rke2Packaging {
			releases[tag] = true
		}
	}

	return releases, nil
}

// ListAssets gets all assets associated with the given release.
func ListAssets(ctx context.Context, client *github.Client, repo, tag string) ([]*github.ReleaseAsset, error) {
	org, err := repository.OrgFromRepo(repo)
	if err != nil {
		return nil, err
	}

	if tag == "" {
		return nil, errors.New("invalid tag provided")
	}

	release, _, err := client.Repositories.GetReleaseByTag(ctx, org, repo, tag)
	if err != nil {
		switch err := err.(type) {
		case *github.ErrorResponse:
			if err.Response.StatusCode != http.StatusNotFound {
				return nil, err
			}
		default:
			return nil, err
		}
	}

	return release.Assets, nil
}

// DeleteAssetsByRelease deletes all release assets for the given release tag.
func DeleteAssetsByRelease(ctx context.Context, client *github.Client, repo, tag string) error {
	org, err := repository.OrgFromRepo(repo)
	if err != nil {
		return err
	}

	if tag == "" {
		return errors.New("invalid tag provided")
	}

	release, _, err := client.Repositories.GetReleaseByTag(ctx, org, repo, tag)
	if err != nil {
		switch err := err.(type) {
		case *github.ErrorResponse:
			if err.Response.StatusCode != http.StatusNotFound {
				return err
			}
		default:
			return err
		}
	}

	for _, asset := range release.Assets {
		if _, err := client.Repositories.DeleteReleaseAsset(ctx, org, repo, asset.GetID()); err != nil {
			return err
		}
	}

	return nil
}

// DeleteAssetByID deletes the release asset associated with the given ID.
func DeleteAssetByID(ctx context.Context, client *github.Client, repo, tag string, id int64) error {
	org, err := repository.OrgFromRepo(repo)
	if err != nil {
		return err
	}

	if tag == "" {
		return errors.New("invalid tag provided")
	}

	if _, err := client.Repositories.DeleteReleaseAsset(ctx, org, repo, id); err != nil {
		return err
	}

	return nil
}

func goModLibVersion(libraryName, repo, branchVersion string) string {
	repoName := "k3s-io/k3s"
	if repo == "rke2" {
		repoName = "rancher/rke2"
	}

	goModURL := "https://raw.githubusercontent.com/" + repoName + "/" + branchVersion + "/go.mod"

	resp, err := http.Get(goModURL)
	if err != nil {
		logrus.Debugf("failed to fetch url %s: %v", goModURL, err)
		return ""
	}
	if resp.StatusCode != http.StatusOK {
		logrus.Debugf("status error: %v when fetching %s", resp.StatusCode, goModURL)
		return ""
	}

	b, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		logrus.Debugf("read body error: %v", err)
		return ""
	}

	modFile, err := modfile.Parse("go.mod", b, nil)
	if err != nil {
		logrus.Debugf("failed to parse go.mod file: %v", err)
		return ""
	}

	// use replace section if found
	for _, replace := range modFile.Replace {
		if strings.Contains(replace.Old.Path, libraryName) {
			return replace.New.Version
		}
	}

	// if replace not found search in require
	for _, require := range modFile.Require {
		if strings.Contains(require.Mod.Path, libraryName) {
			return require.Mod.Version
		}
	}
	logrus.Debugf("library %s not found", libraryName)

	return ""
}

func buildScriptVersion(varName, repo, branchVersion string) string {
	repoName := "k3s-io/k3s"

	if repo == "rke2" {
		repoName = "rancher/rke2"
	}

	buildScriptURL := "https://raw.githubusercontent.com/" + repoName + "/" + branchVersion + "/scripts/version.sh"

	const regex = `(?P<version>v[\d\.]+(-k3s.\w*)?)`
	submatch := findInURL(buildScriptURL, regex, varName)

	if len(submatch) > 1 {
		return submatch[1]
	}

	return ""
}

func dockerfileVersion(chartName, repo, branchVersion string) string {
	if strings.Contains(repo, "k3s") {
		return ""
	}

	const (
		repoName = "rancher/rke2"
		regex    = `(?:FROM|RUN)\s(?:CHART_VERSION=\"|[\w-]+/[\w-]+:)(?P<version>.*?)([0-9][0-9])?(-build.*)?\"?\s`
	)

	dockerfileURL := "https://raw.githubusercontent.com/" + repoName + "/" + branchVersion + "/Dockerfile"

	submatch := findInURL(dockerfileURL, regex, chartName)
	if len(submatch) > 1 {
		return submatch[1]
	}

	return ""
}

func imageTagVersion(ImageName, repo, branchVersion string) string {
	repoName := "k3s-io/k3s"

	imageListURL := "https://raw.githubusercontent.com/" + repoName + "/" + branchVersion + "/scripts/airgap/image-list.txt"
	if repo == "rke2" {
		repoName = "rancher/rke2"
		imageListURL = "https://raw.githubusercontent.com/" + repoName + "/" + branchVersion + "/scripts/build-images"
	}

	const regex = `:(.*)(-build.*)?`
	submatch := findInURL(imageListURL, regex, ImageName)

	if len(submatch) > 1 {
		if strings.Contains(submatch[1], "-build") {
			versionSplit := strings.Split(submatch[1], "-")
			return versionSplit[0]
		}
		return submatch[1]
	}

	return ""
}

func sqliteVersionBinding(sqliteVersion string) string {
	sqliteBindingURL := "https://raw.githubusercontent.com/mattn/go-sqlite3/" + sqliteVersion + "/sqlite3-binding.h"
	const (
		regex = `\"(.*)\"`
		word  = "SQLITE_VERSION"
	)

	submatch := findInURL(sqliteBindingURL, regex, word)
	if len(submatch) > 1 {
		return submatch[1]
	}

	return ""
}

// findInURL will get and scan a url to find a slice submatch for all the words that matches a regex
// if the regex is empty then it will return the lines in a file that matches the str
func findInURL(url, regex, str string) []string {
	var submatch []string

	resp, err := http.Get(url)
	if err != nil {
		logrus.Debugf("failed to fetch url %s: %v", url, err)
		return nil
	}
	if resp.StatusCode != http.StatusOK {
		logrus.Debugf("status error: %v when fetching %s", resp.StatusCode, url)
		return nil
	}

	b, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		logrus.Debugf("read body error: %v", err)
		return nil
	}
	defer resp.Body.Close()

	scanner := bufio.NewScanner(strings.NewReader(string(b)))
	for scanner.Scan() {
		line := scanner.Text()
		if strings.Contains(line, str) {
			if regex == "" {
				submatch = append(submatch, line)
			} else {
				re := regexp.MustCompile(regex)
				submatch = re.FindStringSubmatch(line)
				if len(submatch) > 1 {
					return submatch
				}
			}
		}
	}

	return submatch
}

var changelogTemplate = `
{{- define "changelog" -}}
## Changes since {{.prevMilestone}}:
{{range .content}}
* {{ capitalize .Title }} [(#{{.Number}})]({{.URL}})
{{- $lines := split .Note "\n"}}
{{- range $i, $line := $lines}}
{{- if ne $line "" }}
  * {{ capitalize $line }}
{{- end}}
{{- end}}
{{- end}}
{{- end}}`

const rke2ReleaseNoteTemplate = `
{{- define "rke2" -}}
<!-- {{.milestone}} -->

This release ... <FILL ME OUT!>

**Important Note**

If your server (control-plane) nodes were not started with the ` + "`--token`" + ` CLI flag or config file key, a randomized token was generated during initial cluster startup. This key is used both for joining new nodes to the cluster, and for encrypting cluster bootstrap data within the datastore. Ensure that you retain a copy of this token, as is required when restoring from backup.

You may retrieve the token value from any server already joined to the cluster:
` + "```bash" + `
cat /var/lib/rancher/rke2/server/token
` + "```" + `

{{ template "changelog" . }}

## Packaged Component Versions
| Component       | Version                                                                                           |
| --------------- | ------------------------------------------------------------------------------------------------- |
| Kubernetes      | [{{.k8sVersion}}](https://github.com/kubernetes/kubernetes/blob/master/CHANGELOG/CHANGELOG-{{.majorMinor}}.md#{{.changeLogVersion}}) |
| Etcd            | [{{.EtcdVersionRKE2}}](https://github.com/k3s-io/etcd/releases/tag/{{.EtcdVersionRKE2}})                       |
{{- if eq .majorMinor "1.23"}}
| Containerd      | [{{.ContainerdVersionGoMod}}](https://github.com/k3s-io/containerd/releases/tag/{{.ContainerdVersionGoMod}})                      |
{{- else }}
| Containerd      | [{{.ContainerdVersionRKE2}}](https://github.com/k3s-io/containerd/releases/tag/{{.ContainerdVersionRKE2}})                      |
{{- end }}
| Runc            | [{{.RuncVersionRKE2}}](https://github.com/opencontainers/runc/releases/tag/{{.RuncVersionRKE2}})                              |
| Metrics-server  | [{{.MetricsServerVersion}}](https://github.com/kubernetes-sigs/metrics-server/releases/tag/{{.MetricsServerVersion}})                   |
| CoreDNS         | [{{.CoreDNSVersion}}](https://github.com/coredns/coredns/releases/tag/{{.CoreDNSVersion}})                                  |
| Ingress-Nginx   | [{{.IngressNginxVersion}}](https://github.com/kubernetes/ingress-nginx/releases/tag/helm-chart-{{.IngressNginxVersion}})                                  |
| Helm-controller | [{{.HelmControllerVersion}}](https://github.com/k3s-io/helm-controller/releases/tag/{{.HelmControllerVersion}})                         |

### Available CNIs
| Component       | Version                                                                                                                                                                             | FIPS Compliant |
| --------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | -------------- |
| Canal (Default) | [Flannel {{.FlannelVersionRKE2}}](https://github.com/k3s-io/flannel/releases/tag/{{.FlannelVersionRKE2}})<br/>[Calico {{.CanalCalicoVersion}}](https://projectcalico.docs.tigera.io/archive/{{ majMin .CanalCalicoVersion }}/release-notes/#{{ trimPeriods .CanalCalicoVersion }}) | Yes            |
| Calico          | [{{.CalicoVersion}}](https://projectcalico.docs.tigera.io/archive/{{ majMin .CalicoVersion }}/release-notes/#{{ trimPeriods .CalicoVersion }})                                                                    | No             |
| Cilium          | [{{.CiliumVersion}}](https://github.com/cilium/cilium/releases/tag/{{.CiliumVersion}})                                                                                                                      | No             |
| Multus          | [{{.MultusVersion}}](https://github.com/k8snetworkplumbingwg/multus-cni/releases/tag/{{.MultusVersion}})                                                                                                    | No             |

## Known Issues

- [#1447](https://github.com/rancher/rke2/issues/1447) - When restoring RKE2 from backup to a new node, you should ensure that all pods are stopped following the initial restore:

` + "```" + `bash
curl -sfL https://get.rke2.io | sudo INSTALL_RKE2_VERSION={{.milestone}}
rke2 server \
  --cluster-reset \
  --cluster-reset-restore-path=<PATH-TO-SNAPSHOT> --token <token used in the original cluster>
rke2-killall.sh
systemctl enable rke2-server
systemctl start rke2-server
` + "```" + `

## Helpful Links

As always, we welcome and appreciate feedback from our community of users. Please feel free to:
- [Open issues here](https://github.com/rancher/rke2/issues/new)
- [Join our Slack channel](https://slack.rancher.io/)
- [Check out our documentation](https://docs.rke2.io) for guidance on how to get started.
{{ end }}`

const k3sReleaseNoteTemplate = `
{{- define "k3s" -}}
<!-- {{.milestone}} -->
This release updates Kubernetes to {{.k8sVersion}}, and fixes a number of issues.

For more details on what's new, see the [Kubernetes release notes](https://github.com/kubernetes/kubernetes/blob/master/CHANGELOG/CHANGELOG-{{.majorMinor}}.md#changelog-since-{{.changeLogSince}}).

{{ template "changelog" . }}

## Embedded Component Versions
| Component | Version |
|---|---|
| Kubernetes | [{{.k8sVersion}}](https://github.com/kubernetes/kubernetes/blob/master/CHANGELOG/CHANGELOG-{{.majorMinor}}.md#{{.changeLogVersion}}) |
| Kine | [{{.KineVersion}}](https://github.com/k3s-io/kine/releases/tag/{{.KineVersion}}) |
| SQLite | [{{.SQLiteVersion}}](https://sqlite.org/releaselog/{{.SQLiteVersionReplaced}}.html) |
| Etcd | [{{.EtcdVersionK3S}}](https://github.com/k3s-io/etcd/releases/tag/{{.EtcdVersionK3S}}) |
{{- if eq .majorMinor "1.23"}}
| Containerd | [{{.ContainerdVersionGoMod}}](https://github.com/k3s-io/containerd/releases/tag/{{.ContainerdVersionGoMod}}) |
| Runc | [{{.RuncVersionBuildScript}}](https://github.com/opencontainers/runc/releases/tag/{{.RuncVersionBuildScript}}) |
{{- else }}
| Containerd | [{{.ContainerdVersionK3S}}](https://github.com/k3s-io/containerd/releases/tag/{{.ContainerdVersionK3S}}) |
| Runc | [{{.RuncVersionGoMod}}](https://github.com/opencontainers/runc/releases/tag/{{.RuncVersionGoMod}}) |
{{- end }}
| Flannel | [{{.FlannelVersionK3S}}](https://github.com/flannel-io/flannel/releases/tag/{{.FlannelVersionK3S}}) | 
| Metrics-server | [{{.MetricsServerVersion}}](https://github.com/kubernetes-sigs/metrics-server/releases/tag/{{.MetricsServerVersion}}) |
| Traefik | [v{{.TraefikVersion}}](https://github.com/traefik/traefik/releases/tag/v{{.TraefikVersion}}) |
| CoreDNS | [v{{.CoreDNSVersion}}](https://github.com/coredns/coredns/releases/tag/v{{.CoreDNSVersion}}) | 
| Helm-controller | [{{.HelmControllerVersion}}](https://github.com/k3s-io/helm-controller/releases/tag/{{.HelmControllerVersion}}) |
| Local-path-provisioner | [{{.LocalPathProvisionerVersion}}](https://github.com/rancher/local-path-provisioner/releases/tag/{{.LocalPathProvisionerVersion}}) |

## Helpful Links
As always, we welcome and appreciate feedback from our community of users. Please feel free to:
- [Open issues here](https://github.com/rancher/k3s/issues/new/choose)
- [Join our Slack channel](https://slack.rancher.io/)
- [Check out our documentation](https://rancher.com/docs/k3s/latest/en/) for guidance on how to get started or to dive deep into K3s.
- [Read how you can contribute here](https://github.com/rancher/k3s/blob/master/CONTRIBUTING.md)
{{ end }}`
