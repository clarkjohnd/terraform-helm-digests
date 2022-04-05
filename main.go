package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"regexp"
	"strings"

	"github.com/google/uuid"
	"github.com/hashicorp/go-version"
	dockerparser "github.com/novln/docker-parser"
	yaml "gopkg.in/yaml.v2"
)

type Chart struct {
	Name       string `yaml:"name"`
	Repo       string `yaml:"repo"`
	Url        string `yaml:"url"`
	Version    string `yaml:"version"`
	OldVersion string `yaml:"-"`
}

type V2Response struct {
	Manifests []Manifest `json:"manifests"`
}

type Manifest struct {
	Digest    string `json:"digest"`
	MediaType string `json:"mediaType"`
	Platform  struct {
		Architecture string `json:"architecture"`
		OS           string `json:"os"`
		Variant      string `json:"variant"`
	} `json:"platform"`
}

type Image struct {
	Registry string `yaml:"registry,omitempty"`
	Name     string `yaml:"name"`
	RawName  string `yaml:"-"`
	Digests  struct {
		Amd64 string `yaml:"amd64"`
		Arm64 string `yaml:"arm64"`
	} `yaml:"digests"`
}

type Repo struct {
	Version string `yaml:"version"`
}

// Global environmental variables
var workingDir string
var chartPath string
var imagePath string
var valuePath string
var noWrite string

func main() {

	workingDir = os.Getenv("WORKING_DIRECTORY")
	noWrite = os.Getenv("NO_WRITE")

	chartFile := os.Getenv("CHART_FILE")
	imageFile := os.Getenv("IMAGE_FILE")

	// Environmental variable defaults
	if len(workingDir) == 0 {
		workingDir = "/github/workspace"
	}

	if len(chartFile) == 0 {
		chartFile = "charts.yaml"
	}

	if len(imageFile) == 0 {
		imageFile = "images.yaml"
	}

	// Combine working directory and file names to paths
	chartPath = workingDir + "/" + chartFile
	imagePath = workingDir + "/" + imageFile
	valuePath = workingDir + "/generation/values"

	digestsOnly := os.Getenv("DIGESTS_ONLY")

	if digestsOnly == "" {
		updatedCharts := updateCharts()
		changedImages := generateDigests()

		log.Print("Creating pull request...")
		PullRequest(updatedCharts, workingDir, changedImages)

	} else {
		changedImages := generateDigests()
		if changedImages {
			commitAndPush("", "Updated image digests [ci skip]")
		}
	}

}

func generateDigests() (changedImages bool) {

	// Read the chart YAML file
	log.Printf("Opening file from %s...", chartPath)
	chartYaml, err := os.ReadFile(chartPath)
	check(err)

	// Log the chart YAML File
	MultilineLog(string(chartYaml))

	// Unmarshal the chart YAML file to struct type Chart
	var charts []Chart
	yaml.UnmarshalStrict(chartYaml, &charts)

	// Create the Regular Expression to find Docker images in charts
	// This has to cover YAML parameters other than `image: xxx:1234`
	// This is because images may be referenced in args etc. for operators ` - xxx:1234`
	r := regexp.MustCompile(`(?:\s*?\S*?\s*?)([a-zA-Z\-\.\/\_\\:'\d]+:[a-zA-Z\-\.\'\d]+)+(?:\n|\r)*?`)

	// Create the slice for the raw image paths (REGISTRY/REPO:TAG)
	var allImagesRaw []string

	// Work through each Chart in the charts.yaml file
	for _, chart := range charts {

		// Add the Helm repository and update
		log.Printf("Getting %s Helm repository from %s", chart.Name, chart.Url)
		Command(fmt.Sprintf("helm repo add %s %s", chart.Repo, chart.Url), "")
		Command(fmt.Sprintf("helm repo update %s", chart.Repo), "")

		// Create command to generate the YAML templates to scan for the repo
		cmdString := fmt.Sprintf(
			"helm template %s %s/%s --version %s --include-crds",
			chart.Name,
			chart.Repo,
			chart.Name,
			chart.Version,
		)

		// Create the path to a values file that may be used when templating
		valuesFile := fmt.Sprintf("%s/%s.yaml", valuePath, chart.Name)

		// Check if the values file exists, if so add it to the template command
		if _, err := os.Stat(valuesFile); !errors.Is(err, os.ErrNotExist) {
			cmdString += " --values " + valuesFile
		}

		// Run the template command and store the YAML template as string
		template := Command(cmdString, workingDir)

		// Look for all RegEx matches in the YAML template
		// Output format: [[ WHOLE_LINE, IMAGE_MATCH ], ...]
		matches := r.FindAllStringSubmatch(string(template), -1)

		// Create slice for images from RegEx match
		var imagesRaw []string

		// Pull the image path (discarding the whole line match)
		for _, image := range matches {
			imagesRaw = append(imagesRaw, image[1])
		}

		// Append images from this chart to all other images
		allImagesRaw = append(allImagesRaw, imagesRaw...)

	}

	// Remove duplicate images
	allImagesRaw = unique(allImagesRaw)

	if len(allImagesRaw) > 0 {
		changedImages = true
	} else {
		changedImages = false
	}

	// Process image paths to seperate registry, repo, name, tags
	chartImages := getImageData(allImagesRaw)

	// Login to each registry (currently Docker, Quay.io, GCR and ECR public)
	registryLogin(chartImages)

	// Pull the AMD and ARM digests for all the images
	chartImages = getDigests(chartImages)

	// Create the images YAML struct with registry, name, digests
	outputImages, err := yaml.Marshal(chartImages)
	check(err)

	// Write the YAML struct to the images.yaml file
	err = os.WriteFile(imagePath, outputImages, 0666)
	check(err)

	// Log the output YAML string
	MultilineLog(string(outputImages))

	return

}

func updateCharts() []Chart {

	log.Printf("Opening file from %s...", chartPath)
	chartYaml, err := os.ReadFile(chartPath)

	check(err)

	MultilineLog(string(chartYaml))

	updates := false
	var updatedCharts []Chart

	var chartsTmp []Chart

	yaml.UnmarshalStrict(chartYaml, &chartsTmp)

	charts := chartsTmp

	for i, chart := range charts {

		log.Printf("Getting %s Helm repository from %s", chart.Name, chart.Url)

		helmCommands := []string{
			fmt.Sprintf("helm repo add %s %s", chart.Repo, chart.Url),
			fmt.Sprintf("helm repo update %s", chart.Repo),
		}

		for _, cmd := range helmCommands {
			Command(cmd, "")
		}

		log.Printf("Pulling %s versions", chart.Name)

		versionYaml := Command(fmt.Sprintf("helm search repo %s/%s -l -o yaml", chart.Repo, chart.Name), "")

		var repo []Repo

		yaml.UnmarshalStrict(versionYaml, &repo)

		currentVersion, _ := version.NewVersion(chart.Version)
		latestVersion, _ := version.NewVersion(repo[0].Version)

		if currentVersion.LessThan(latestVersion) {
			log.Printf("Found newer version: %s", latestVersion)
			charts[i].OldVersion = currentVersion.Original()
			charts[i].Version = latestVersion.Original()
			updatedCharts = append(updatedCharts, charts[i])
			updates = true
		} else {
			log.Printf("Current version %s latest", currentVersion)
		}

	}

	if !updates {
		log.Print("No newer versions found, nothing to do.")
		os.Exit(0)
	}

	if len(noWrite) > 0 {
		log.Print("NO_WRITE environmental variable set, preventing file writing and pull request")
		os.Exit(0)
	}

	log.Print("Newer versions found, updating charts.yaml")

	newChartsYaml, err := yaml.Marshal(charts)
	check(err)

	err = os.WriteFile(chartPath, newChartsYaml, 0666)
	check(err)

	log.Print("Written new chart version configuration to charts.yaml:")
	MultilineLog(string(newChartsYaml))

	return updatedCharts

}

// A function to split and log multiline strings.
func MultilineLog(input string) {

	lines := strings.Split(input, "\n")

	for _, line := range lines {
		log.Print(line)
	}
}

// A function to run a command by string in a specific working directory
// with stdout and stderr logging and error checks.
func Command(inputString string, dir string) []byte {

	MultilineLog(fmt.Sprintf("$ %s", inputString))

	quoted := false
	input := strings.FieldsFunc(inputString, func(r rune) bool {
		if r == '"' {
			quoted = !quoted
		}
		return !quoted && r == ' '
	})

	for i, s := range input {
		input[i] = strings.Trim(s, `"`)
	}

	cmd := exec.Command(
		input[0], input[1:]...,
	)

	if dir != "" || len(dir) > 0 {
		cmd.Dir = dir
	}

	var out bytes.Buffer
	var stderr bytes.Buffer

	cmd.Stdout = &out
	cmd.Stderr = &stderr

	err := cmd.Run()

	if err != nil {
		MultilineLog(fmt.Sprint(err) + ": " + stderr.String())
		log.Fatal(err)
	}

	MultilineLog("Result: " + out.String())

	return out.Bytes()
}

// Simple function to check errors.
func check(err error) {
	if err != nil {
		log.Fatal(err)
	}
}

// Removes duplicate strings from a string slice.
func unique(s []string) []string {
	inResult := make(map[string]bool)
	var result []string
	for _, str := range s {
		if _, ok := inResult[str]; !ok {
			inResult[str] = true
			result = append(result, str)
		}
	}
	return result
}

/*
Pulls the digests for a slice of images in the following format:
	Image:
		Registry: image registry (k8s.gcr.io etc.)
		Name:	 image name (external-dns/external-dns etc.)
		RawName:  registry, name, tag (k8s.gcr.io/external-dns/external-dns:v0.10.2)

Adds the digests to the same format:
	Image:
		...
		Digests:
			Amd64: Digest for AMD64 image
			Arm64: Digest for ARM64 image
*/
func getDigests(images []Image) []Image {

	for i, image := range images {

		regPath := os.Getenv("REG_PATH")

		if len(regPath) == 0 {
			regPath = "reg"
		}

		rawV2Response := Command(fmt.Sprintf("%s manifest %s", regPath, image.RawName), "")

		var v2Response V2Response

		json.Unmarshal(rawV2Response, &v2Response)

		foundAmd := false
		foundArm := false

		for _, manifest := range v2Response.Manifests {

			if manifest.Platform.Architecture == "amd64" {

				image.Digests.Amd64 = manifest.Digest
				foundAmd = true

			} else if manifest.Platform.Architecture == "arm64" {

				image.Digests.Arm64 = manifest.Digest
				foundArm = true

			}
		}

		if !foundAmd || !foundArm {
			log.Print("Failed to find both ARM and AMD digests for image " + image.RawName)
			log.Fatal()
		}

		images[i].Digests = image.Digests

	}

	return images

}

// Pull image parameters from raw image paths
func getImageData(imagesRaw []string) (images []Image) {

	for _, imageRaw := range imagesRaw {

		var imageStruct Image

		imageData, _ := dockerparser.Parse(imageRaw)

		// Registry: quay.io
		registry := imageData.Registry()
		// Short name: argoproj/argoexec
		shortName := imageData.ShortName()

		imageStruct.Name = shortName

		if registry != "docker.io" {
			imageStruct.Registry = registry
		}

		imageStruct.RawName = imageRaw

		images = append(images, imageStruct)
	}

	return

}

// Function to login to registries
func registryLogin(images []Image) {

	// Each registry appended to this list when logged in
	var loggedInList []string

	for _, image := range images {

		registry := image.Registry

		if registry == "quay.io" {
			// Login for quay.io registry (Argo etc.)
			// QUAY_USERNAME and QUAY_PASSWORD env variables required
			if !checkList(loggedInList, registry) {
				stdinLogin(registry, os.Getenv("QUAY_USERNAME"), os.Getenv("QUAY_PASSWORD"))
				loggedInList = append(loggedInList, registry)
			}

		} else if strings.Contains(registry, "gcr.io") {
			// Login for any public gcr.io registry
			// GCR_JSON_KEY for a GCR IAM role env variable required
			if !checkList(loggedInList, registry) {
				stdinLogin(registry, "_json_key", os.Getenv("GCR_JSON_KEY"))
				loggedInList = append(loggedInList, registry)
			}

		} else if strings.Contains(registry, ".ecr.") {
			// Login for any public ECR registry
			// AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY env variable required
			// User/role required policies:
			// 		ecr:BatchGetImage
			//		ecr:GetAuthorizationToken
			if !checkList(loggedInList, registry) {
				stdinLogin(registry, "", "")
				loggedInList = append(loggedInList, registry)
			}
		} else if registry != "" {
			// If registry not Docker or the above configs:
			log.Print("No login configuration for " + registry)
			log.Fatal()
		}
	}

}

// Returns true if checkEntry is present in list, otherwise false
func checkList(list []string, checkEntry string) bool {

	for _, entry := range list {
		if checkEntry == entry {
			return true
		}
	}
	return false
}

// This function is used to login to registries by passing passwords through a
// stdin reader and using the "docker login ... --password-stdin" command.
// Passwords are not logged.
func stdinLogin(registry string, username string, password string) {

	// Additional commands for ECR registries.
	// AWS CLI on Docker image used to generate a temporary password.
	// Password then provided to Docker login command.
	// AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY envs MUST be set
	if strings.Contains(registry, "ecr") {

		// Pull ECR region for use with AWS CLI
		registryWords := strings.Split(registry, ".")
		region := registryWords[len(registryWords)-3]

		os.Setenv("AWS_REGION", region)

		// Generate ECR password with CLI
		newPassword, err := exec.Command("aws", "ecr", "get-login-password", "--region", region).Output()
		check(err)

		username = "AWS"
		password = string(newPassword)

	}

	// Pass password into command and run
	cmd := exec.Command("docker", "login", registry, "--username", username, "--password-stdin")
	cmd.Stdin = strings.NewReader(password)
	var out bytes.Buffer
	var stderr bytes.Buffer

	cmd.Stdout = &out
	cmd.Stderr = &stderr

	err := cmd.Run()

	// Log error if present
	if err != nil {
		MultilineLog(fmt.Sprint(err) + ": " + stderr.String())
		log.Fatal(err)
	}

	MultilineLog(registry + " login result: " + out.String())
}

func gitConfig() {

	ghToken := os.Getenv("GITHUB_TOKEN")
	ghRepo := os.Getenv("GITHUB_REPOSITORY")
	ghActor := os.Getenv("GITHUB_ACTOR")

	if len(ghToken) == 0 {
		log.Print("No GitHub token (env GITHUB_TOKEN) provided")
		os.Exit(1)
	}

	if len(ghRepo) == 0 {
		log.Print("No GitHub repository (env GITHUB_REPOSITORY) provided")
		os.Exit(1)
	}

	if len(ghActor) == 0 {
		log.Print("No GitHub actor (env GITHUB_ACTOR) provided")
		os.Exit(1)
	}

	Command(fmt.Sprintf(`git config user.name "%s"`, ghActor), workingDir)
	Command(`git config user.email "<>"`, workingDir)
}

func commitAndPush(branchName string, message string) (changes bool) {

	gitConfig()

	if branchName != "" {
		log.Print(fmt.Sprintf("Creating new branch %s...", branchName))
		Command(fmt.Sprintf("git checkout -b %s", branchName), workingDir)
		log.Print("Branch sucessfully created!")
	}

	log.Print("Committing changes to remote branch...")

	gitStatus := string(Command("git status --porcelain", workingDir))

	if gitStatus == "" {
		log.Print("No changes, exiting")
		changes = false
		return
	}

	Command("git add -A", workingDir)
	Command(fmt.Sprintf(`git commit -m "%s"`, message), workingDir)

	if branchName != "" {
		Command(fmt.Sprintf("git push -u origin %s", branchName), workingDir)
	} else {
		Command("git push", workingDir)
	}

	log.Print("Successfully pushed changes to remote branch!")
	changes = true
	return changes

}

func PullRequest(charts []Chart, workingDir string, changedImages bool) {

	branchName := fmt.Sprintf("helm-update-%s", uuid.New().String()[0:6])
	pullRequestTitle := "Bump "
	pullRequestBody := "## Terraform Helm Updater\n"

	for _, chart := range charts {
		pullRequestTitle += fmt.Sprintf(
			"%s from %s to %s, ",
			chart.Name,
			chart.OldVersion,
			chart.Version,
		)

		pullRequestBody += fmt.Sprintf(
			"Bumps %s Helm Chart version from %s to %s.\n",
			chart.Name,
			chart.OldVersion,
			chart.Version,
		)
	}

	if changedImages {
		pullRequestBody += "\n---\nAlso updated list of image digests."
	}

	pullRequestTitle = pullRequestTitle[:len(pullRequestTitle)-2]

	log.Print("Pull Request Title:")
	log.Print(pullRequestTitle)
	log.Print("Pull Request Body:")
	MultilineLog(pullRequestBody)

	noPR := os.Getenv("NO_PR")

	if len(noPR) > 0 {
		log.Print("NO_PR environmental variable set, preventing pull request")
		return
	}

	changes := commitAndPush(branchName, "Updated chart versions")

	if !changes {
		log.Print("No changes for a pull request")
		return
	}

	mainBranch := os.Getenv("MAIN_BRANCH")

	if len(mainBranch) == 0 {
		mainBranch = "main"
	}

	log.Print("Creating pull request...")
	Command(
		fmt.Sprintf(
			`gh pr create -t "%s" -b "%s" -B %s -H %s -l "dependencies" -l "github_actions"`,
			pullRequestTitle,
			pullRequestBody,
			mainBranch,
			branchName,
		),
		workingDir,
	)
	log.Print("Successfully created pull request!")

}
