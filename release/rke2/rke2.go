package rke2

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/go-github/v39/github"
	"github.com/rancher/ecm-distro-tools/docker"
	"github.com/rancher/ecm-distro-tools/exec"
	ecmHTTP "github.com/rancher/ecm-distro-tools/http"
	"github.com/sirupsen/logrus"
)

const (
	goDevURL                       = "https://go.dev/dl/?mode=json"
	imageBuildBaseRepo             = "image-build-base"
	updateImageBuildScriptFileName = "update_image_build_base.sh"
	updateImageBuildScript         = `#!/bin/sh
set -e
REPO_NAME={{ .RepoName }}
REPO_ORG={{ .RepoOrg }}
DRY_RUN={{ .DryRun }}
CLONE_DIR={{ .CloneDir }}
NEW_TAG={{ .NewTag }}
BRANCH_NAME={{ .BranchName }}
echo "repo name: ${REPO_NAME}"
echo "org name: ${REPO_ORG}"
echo "dry run: ${DRY_RUN}"
echo "current tag: ${CURRENT_TAG}"
echo "branch name: ${BRANCH_NAME}"

echo "cloning ${REPO_ORG}/${REPO_NAME} into ${CLONE_DIR}"
git clone "git@github.com:${REPO_ORG}/${REPO_NAME}.git" "${CLONE_DIR}"
echo "navigating to the repo dir"
cd "${CLONE_DIR}"
CURRENT_TAG=$(cat .hardened-build-base-version)
echo "new tag: ${NEW_TAG}"
echo "creating local branch"
git checkout -B "${BRANCH_NAME}" master
git clean -xfd
OS=$(uname -s)
case ${OS} in
Darwin)
	sed -i '' "s/hardened-build-base:${CURRENT_TAG}/hardened-build-base:${NEW_TAG}/" Dockerfile
	;;
Linux)
	sed -i "s/hardened-build-base:${CURRENT_TAG}/hardened-build-base:${NEW_TAG}/" Dockerfile
	;;
*)
	>&2 echo "$(OS) not supported yet"
	exit 1
	;;
esac
git add Dockerfile
git commit -m "update hardened-build-base to ${NEW_TAG}"
if [ "${DRY_RUN}" = false ]; then
	git push --set-upstream origin ${BRANCH_NAME}
fi`
)

type UpdateImageBuildArgs struct {
	RepoName   string
	RepoOwner  string
	BranchName string
	DryRun     bool
	CloneDir   string
	NewTag     string
}

var imageBuildRepos map[string]bool = map[string]bool{
	"image-build-dns-nodecache":                    true,
	"image-build-k8s-metrics-server":               true,
	"image-build-sriov-cni":                        true,
	"image-build-ib-sriov-cni":                     true,
	"image-build-sriov-network-device-plugin":      true,
	"image-build-sriov-network-resources-injector": true,
	"image-build-calico":                           true,
	"image-build-cni-plugins":                      true,
	"image-build-whereabouts":                      true,
	"image-build-flannel":                          true,
	"image-build-etcd":                             true,
	"image-build-containerd":                       true,
	"image-build-runc":                             true,
	"image-build-multus":                           true,
	"image-build-rke2-cloud-provider":              true,
}

type goVersionRecord struct {
	Version string `json:"version"`
	Stable  bool   `json:"stable"`
}

func ImageBuildBaseRelease(ctx context.Context, ghClient *github.Client, alpineVersion string, dryRun bool) error {
	versions, err := goVersions(goDevURL)
	if err != nil {
		return err
	}

	for _, version := range versions {
		logrus.Info("version: " + version.Version)
		if !version.Stable {
			logrus.Info("version " + version.Version + " is not stable")
			continue
		}
		version := strings.Split(version.Version, "go")[1]
		alpineTag := version + "-alpine" + alpineVersion

		if err := docker.CheckImageArchs(ctx, "library", "golang", alpineTag, []string{"amd64", "arm64", "s390x"}); err != nil {
			return err
		}

		imageBuildBaseTag := "v" + version + "b1"
		logrus.Info("stripped version: " + imageBuildBaseTag)
		if _, _, err := ghClient.Repositories.GetReleaseByTag(ctx, "rancher", imageBuildBaseRepo, imageBuildBaseTag); err == nil {
			logrus.Info("release " + imageBuildBaseTag + " already exists")
			continue
		}
		logrus.Info("release " + imageBuildBaseTag + " doesn't exists, creating release")
		if dryRun {
			logrus.Info("dry run, release won't be created")
			logrus.Infof("Release:\n  Owner: rancher\n  Repo: %s\n  TagName: %s\n  Name: %s\n", imageBuildBaseRepo, imageBuildBaseTag, imageBuildBaseTag)
			return nil
		}
		release := &github.RepositoryRelease{
			TagName:    github.String(imageBuildBaseTag),
			Name:       github.String(imageBuildBaseTag),
			Prerelease: github.Bool(false),
		}
		if _, _, err := ghClient.Repositories.CreateRelease(ctx, "rancher", imageBuildBaseRepo, release); err != nil {
			return err
		}
		logrus.Info("created release for version: " + imageBuildBaseTag)
	}
	return nil
}

func goVersions(goDevURL string) ([]goVersionRecord, error) {
	httpClient := ecmHTTP.NewClient(time.Second * 15)
	res, err := httpClient.Get(goDevURL)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		return nil, errors.New("failed to get stable go versions")
	}

	var versions []goVersionRecord
	if err := json.NewDecoder(res.Body).Decode(&versions); err != nil {
		return nil, err
	}
	return versions, nil
}

func UpdateImageBuild(ctx context.Context, ghClient *github.Client, repo, owner, cloneDir string, dryRun, createPR bool) error {
	if _, ok := imageBuildRepos[repo]; !ok {
		return errors.New("invalid repo, please review the `imageBuildRepos` map")
	}
	newTag, err := latestTag(ctx, ghClient, owner, repo)
	if err != nil {
		return err
	}
	branchName := "update-to-" + newTag
	data := UpdateImageBuildArgs{
		RepoName:   repo,
		RepoOwner:  owner,
		BranchName: branchName,
		DryRun:     dryRun,
		CloneDir:   cloneDir,
		NewTag:     newTag,
	}
	output, err := exec.RunTemplatedScript(cloneDir, updateImageBuildScriptFileName, updateImageBuildScript, data)
	if err != nil {
		return err
	}
	logrus.Info(output)
	if createPR {
		prName := "Update hardened build base to " + newTag
		logrus.Info("preparing PR")
		if dryRun {
			logrus.Info("dry run, PR will not be created")
			logrus.Info("PR:\n  Name: " + prName + "\n  From: " + owner + ":" + branchName + "\n  To rancher:master")
			return nil
		}
		if err := createPRFromRancher(ctx, ghClient, prName, branchName, owner, repo); err != nil {
			return err
		}
	}
	return nil
}

func createPRFromRancher(ctx context.Context, ghClient *github.Client, title, branchName, forkOwner, repo string) error {
	pull := &github.NewPullRequest{
		Title:               &title,
		Base:                github.String("master"),
		Head:                github.String(forkOwner + ":" + branchName),
		MaintainerCanModify: github.Bool(true),
	}
	_, _, err := ghClient.PullRequests.Create(ctx, "rancher", repo, pull)
	return err
}

func latestTag(ctx context.Context, ghClient *github.Client, owner, repo string) (string, error) {
	release, _, err := ghClient.Repositories.GetLatestRelease(ctx, owner, repo)
	if err != nil {
		return "", err
	}
	return *release.TagName, nil
}
