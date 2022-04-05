# Terraform Helm Digests

Use this action for two functions:

- Update Helm charts in a Terraform module
- Pull image digests to be used in the chart

Terraform modules that wish to use this action should have a ```charts.yaml``` file in place. An ```images.yaml``` file will be generated, but the module itself should be setup to use these two files and merge them into locals using ```yamldecode(file("./charts.yaml"))``` etc.

Below is an example ```charts.yaml``` file:

```yaml
- name: argo-workflows
  repo: argo
  url: https://argoproj.github.io/argo-helm
  version: 0.13.1
- name: argo-events
  repo: argo
  url: https://argoproj.github.io/argo-helm
  version: 1.12.0
```

Below is an example ```images.yaml``` file:

```yaml
- registry: quay.io
  name: argoproj/workflow-controller
  digests:
    amd64: sha256:2b8715e71040e332b0faea68f0724bf7c6e193a61d5babecac03a6d68fe86efc
    arm64: sha256:614275080106101e49c8227a98d12951a73a4c3a9456dc684cff8775a543a35c
- registry: quay.io
  name: argoproj/argoexec
  digests:
    amd64: sha256:aeb0699e6a586338403d3baa7b76b7c33b8478e04ba13420b0bc60ef78d9f6f5
    arm64: sha256:1a90d6055480de3f3d1288fd6b8093079ec6fefc4d63dc8cc6510e42dded356f
```

## Updating Helm chart versions

This part of the action will:

- Parse each chart with the current version
- Check for latest versions
- Update the version reference if a newer one is available to ```charts.yaml```
- Pull the image digests (see next section)
- Create a pull request like Dependabot does.

Ideally, this PR will be subject to automated CICD linting and unit tests followed by automated approval and merging.

The only variable this action needs in a GitHub Actions workflow is GITHUB_TOKEN. A full implementation example is shown at the end.

## Pulling image digests

For security reasons, it's always preferred to use immutable image digests (SHA256:xxxx) instead of tags. Tags can be overwritten, so the same Helm deployment with the same tag reference can in theory deploy different images if the image developers are not careful with tags. Digests cannot be overwritten, so providing the digest does not change, the image deployed will always be the same image.

This part of the action will:

- Parse each chart with the current version
- Download the charts
- Generate the YAML templates from the chart
- Parse each template looking for image references with tags
- Pulls DockerV2 API manifests for each of the images found
- Stores the AMD64 and ARM64 image digests to the ```images.yaml``` file

This action uses [Reg](https://github.com/genuinetools/reg) to pull manifest data for Docker images. Although generally the images used are placed in public registries (```quay.io```, ```k8s.gcr.io```, ```602401143452.dkr.ecr.us-west-2.amazonaws.com``` etc.), even these public registries require a "login" to access using the Docker V2 API, which Reg uses. To access the following registries, certain credentials are required, which can be stored as repository secrets and passed as environmental variables to the action. Ideally, all credentials used should be linked to accounts that are standalone and isolated away from any other Quay.io, AWS or GCP accounts.

### quay.io

```QUAY_USERNAME```

- username for quay.io

```QUAY_PASSWORD```

- A password for quay.io

### xxxxx.gcr.io

```GCR_JSON_KEY```

- This is a JSON file for a Compute Engine service account downloaded from the Google Cloud Platform.  The role of the service account should be ```Container Registry Service Agent```.

### xxxxx.ecr.REGION.amazonaws.com

```AWS_ACCESS_KEY_ID```

- An IAM user access ID key

```AWS_SECRET_ACCESS_KEY```

- An IAM user secret access key

ECR registries cannot be logged into with static credentials. For this reason, the AWS CLI is included in this action. AWS credentials should therefore be passed into the action. The AWS CLI will generate a temporary token which is used to login to ECR registries. The AWS IAM user/role should have the following permissions:

```json
{
    "Version": "2012-10-17",
    "Statement": [
        {
            "Sid": "VisualEditor0",
            "Effect": "Allow",
            "Action": [
                "ecr:BatchGetImage",
                "ecr:GetAuthorizationToken"
            ],
            "Resource": "*"
        }
    ]
}
```

### Docker.io

No further action required for public Docker registries.

## Helm Version Dependency Upgrades

Below is a workflow example of a Dependabot-like configuration, where the action will try update the Helm versions, update the digests, and create a PR.

```yaml
name: Update Helm Charts
on:
  schedule:
    - cron:  '0 7 * * 7'
  workflow_dispatch:

jobs:
  autoupdate:
    name: Update Helm releases in Terraform modules
    runs-on: ubuntu-latest
    steps:
      - name: Checkout Code
        uses: actions/checkout@v3

      - name: Helm version update
        uses: clarkjohnd/terraform-helm-digests@v0.0.1
        with:
          github-token: ${{ secrets.GITHUB_TOKEN }}
          quay-username: ${{ secrets.QUAY_USERNAME }}
          quay-password: ${{ secrets.QUAY_PASSWORD }}
          gcr-json-key: ${{ secrets.GCR_JSON_KEY }}
          aws-access-key-id: ${{ secrets.AWS_ECR_ACCESS_KEY_ID }}
          aws-secret-access-key: ${{ secrets.AWS_ECR_SECRET_ACCESS_KEY }}
```

In the event that developers may update the Helm chart versions themselves, we can configure a workflow to update the digests every time there is a push which changes either the chart versions or the image file in a development branch. We will set the ```DIGESTS_ONLY``` environmental variable to true which does not try update the chart file. The workflow will not run if no changes are made to either of the YAML files or if it's a push/PR to main.

```yaml
name: Update Image Digests
on:
  push:
    paths:
      - 'charts.yaml' # Get digests on manual charts.yaml change
      - 'images.yaml' # Get digests if someone has changed this file
    branches-ignore:
      - 'main'        # Ignore when PR'd into main
      - 'd/helm/**'   # Ignore Helm version upgrade branches
  workflow_dispatch:

jobs:
  digests:
    name: Update Helm releases in Terraform modules
    runs-on: ubuntu-latest
    if: "!contains(github.event.head_commit.message, '[ci skip]')"
    steps:
      - name: Checkout Code
        uses: actions/checkout@v3

      - name: Helm version update
        uses: clarkjohnd/terraform-helm-digests@v0.0.1
        with:
          digests-only: "true"
          github-token: ${{ secrets.GITHUB_TOKEN }}
          quay-username: ${{ secrets.QUAY_USERNAME }}
          quay-password: ${{ secrets.QUAY_PASSWORD }}
          gcr-json-key: ${{ secrets.GCR_JSON_KEY }}
          aws-access-key-id: ${{ secrets.AWS_ECR_ACCESS_KEY_ID }}
          aws-secret-access-key: ${{ secrets.AWS_ECR_SECRET_ACCESS_KEY }}
```
