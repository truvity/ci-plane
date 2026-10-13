package runneridentity

import (
	"encoding/json"
	"fmt"
)

// IAM policy vocabulary. Documents are built as maps and marshalled with
// encoding/json, which orders keys: the same inputs always yield the same
// bytes, so an unchanged grant is never an update.
const (
	polVersion   = "Version"
	polStatement = "Statement"
	polSid       = "Sid"
	polEffect    = "Effect"
	polAllow     = "Allow"
	polAction    = "Action"
	polResource  = "Resource"
	polCondition = "Condition"
	polStringLik = "StringLike"
	polDocDate   = "2012-10-17"

	ecrBatchGetImage   = "ecr:BatchGetImage"
	ecrGetDownloadURL  = "ecr:GetDownloadUrlForLayer"
	ecrBatchCheckLayer = "ecr:BatchCheckLayerAvailability"
	ecrGetAuthToken    = "ecr:GetAuthorizationToken"
	ecrPutImage        = "ecr:PutImage"

	// partition is the ARN partition. Built into every ARN at run time;
	// the repository's leak canary refuses a literal ARN prefix anywhere.
	partition = "aws"

	s3ListBucket   = "s3:ListBucket"
	s3GetObject    = "s3:GetObject"
	s3PutObject    = "s3:PutObject"
	s3DeleteObject = "s3:DeleteObject"
)

// statement is one policy statement.
type statement = map[string]any

// document marshals statements into a policy document.
func document(statements []statement) string {
	raw, err := json.Marshal(map[string]any{
		polVersion:   polDocDate,
		polStatement: statements,
	})
	if err != nil {
		panic(err) // maps of strings and slices of strings cannot fail to marshal
	}

	return string(raw)
}

// ecrRepoARN is the ARN of a repository name or pattern in the estate's
// registry account.
func (e Estate) ecrRepoARN(repo string) string {
	return fmt.Sprintf("arn:%s:ecr:%s:%s:repository/%s", partition, e.Region, e.AccountID, repo)
}

// s3ARN is the ARN of a bucket, or of a key pattern under it (a name with a
// slash).
func s3ARN(name string) string {
	return "arn:" + partition + ":s3:::" + name
}

// ecrRepoARNs maps ecrRepoARN over repos.
func (e Estate) ecrRepoARNs(repos []string) []string {
	out := make([]string, 0, len(repos))
	for _, r := range repos {
		out = append(out, e.ecrRepoARN(r))
	}

	return out
}

// ecrRead is the three actions of an image or chart pull. The
// authorization token is a separate statement because it cannot be scoped
// to a resource.
func ecrRead() []string {
	return []string{ecrBatchGetImage, ecrGetDownloadURL, ecrBatchCheckLayer}
}

// runnerImagesStatement lets a pool pull ITS OWN scale-set images: the
// runner-image repositories, read-only. No build pulls them.
func runnerImagesStatement(e Estate, repos []string) statement {
	return statement{
		polSid:      "EcrRunnerImages",
		polEffect:   polAllow,
		polAction:   ecrRead(),
		polResource: e.ecrRepoARNs(repos),
	}
}

// stableChartsReadStatement lets CI pull released Helm charts from the
// registry that publishes them: the three layer/manifest actions a
// `helm pull` of an OCI chart makes, on named repositories, never the
// account. ecr:GetAuthorizationToken is not here: it cannot be scoped to a
// resource and the shared policy attached to the primary pool carries it.
func stableChartsReadStatement(repoARNs []string) statement {
	return statement{
		polSid:      "EcrStableChartsRead",
		polEffect:   polAllow,
		polAction:   ecrRead(),
		polResource: repoARNs,
	}
}

// cacheStatements are the Go build cache's two statements on the CI cache
// bucket: list it, and read, write and delete objects in it. Deliberately
// POOL-ONLY: the cache is the runner's infrastructure and no federated
// role holds it (a credential-bearing [default] profile in a CI AWS config
// shadows this identity and silently degrades the cache).
func cacheStatements(bucket string) []statement {
	return []statement{
		{
			polSid:      "GoBuildCacheList",
			polEffect:   polAllow,
			polAction:   s3ListBucket,
			polResource: s3ARN(bucket),
		},
		{
			polSid:      "GoBuildCacheObjects",
			polEffect:   polAllow,
			polAction:   []string{s3GetObject, s3PutObject, s3DeleteObject},
			polResource: s3ARN(bucket) + "/*",
		},
	}
}

// pullThroughStatements are a pool's own registry auth and read-only
// pull-through union, for a pool whose registry grants are NOT attached from
// a shared policy: restated rather than shared, because the shared policy
// also carries a push the pool must not have.
func pullThroughStatements(e Estate, repos []string) []statement {
	return []statement{
		{
			polSid:      "EcrAuth",
			polEffect:   polAllow,
			polAction:   ecrGetAuthToken,
			polResource: "*",
		},
		{
			polSid:    "EcrPullThroughRead",
			polEffect: polAllow,
			polAction: append(ecrRead(),
				"ecr:BatchImportUpstreamImage",
				// The first pull of an image nobody pulled before creates
				// its local repository (a build's SBOM scanner image is such
				// a pull); the node role carries the same pair.
				"ecr:CreateRepository",
			),
			polResource: e.ecrRepoARNs(repos),
		},
	}
}

// buildkitCacheStatement is a pool's share of the BuildKit warm-cache
// repository: pull and push of that one repository and nothing else. The
// shared in-cluster builders push `--cache-to type=registry` with the
// CLIENT's registry credentials, so the runner's identity needs the push.
func buildkitCacheStatement(e Estate, repo string) statement {
	return statement{
		polSid:    "EcrBuildkitCache",
		polEffect: polAllow,
		polAction: []string{
			ecrPutImage,
			"ecr:InitiateLayerUpload",
			"ecr:UploadLayerPart",
			"ecr:CompleteLayerUpload",
			ecrBatchCheckLayer,
			ecrBatchGetImage,
			ecrGetDownloadURL,
			"ecr:DescribeRepositories",
			"ecr:ListImages",
			"ecr:DescribeImages",
		},
		polResource: e.ecrRepoARN(repo),
	}
}
