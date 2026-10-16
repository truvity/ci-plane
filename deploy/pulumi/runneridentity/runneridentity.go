package runneridentity

import (
	"errors"
	"fmt"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/truvity/k8s/pkg/aws/podidentity"
)

type (
	// Estate is the account and cluster the identities live in.
	Estate struct {
		// Provider is the AWS provider of the account and region. Required.
		Provider pulumi.ProviderResource
		// Cluster is the EKS cluster name the Pod Identity associations are
		// made in. Required.
		Cluster string
		// Region and AccountID address the registry's repositories and pin
		// each role's trust policy to the cluster. Required.
		Region    string
		AccountID string
		// BoundaryARN is the permissions boundary of every role. Empty sets
		// none.
		BoundaryARN string
	}

	// Registry says where a pool's registry grants come from.
	Registry int

	// Option shapes the pod-identity component a constructor registers. It
	// is the hook an estate uses to keep the logical names and URNs its
	// resources already have.
	Option func(*podidentity.Args)

	// Pool is one runner pool's plumbing identity: the role the runner pods
	// of one scale-set namespace assume. Plumbing means pulling images,
	// reading the registry's pull-through caches and using the disposable CI
	// cache bucket, never anything deploy-shaped.
	Pool struct {
		Estate

		// RoleName, PolicyName and Description name the IAM role and its
		// managed policy. A policy's description is immutable: changing it
		// replaces the policy, so an adopted pool passes the original.
		RoleName    string
		PolicyName  string
		Description string
		// Namespace and ServiceAccount are the pods that may assume it.
		Namespace      string
		ServiceAccount string

		// Registry selects the statement set; see its constants.
		Registry Registry

		// RunnerImageRepos are the repository names (or patterns) the pool
		// pulls its own runner images from. Required.
		RunnerImageRepos []string
		// CacheBucket is the CI cache bucket's name. Required.
		CacheBucket string

		// StableChartRepoARNs are the ARNs of the chart repositories CI may
		// read (RegistryShared only; required there).
		StableChartRepoARNs []string

		// PullThroughRepos are the pull-through repositories the pool may
		// read (RegistryOwn only; required there).
		PullThroughRepos []string
		// BuildkitCacheRepo is the shared BuildKit warm-cache repository
		// (RegistryOwn only). Empty grants none.
		BuildkitCacheRepo string
	}

	// Modproxy is the Go module proxy's identity: one prefix of the CI cache
	// bucket and nothing else.
	Modproxy struct {
		Estate

		RoleName       string
		PolicyName     string
		Description    string
		Namespace      string
		ServiceAccount string

		// Bucket is the CI cache bucket and Prefix the key prefix the proxy
		// stores module zips and sumdb responses under (no slashes).
		Bucket string
		Prefix string
	}

	// CacheRedirect is cache-redirect's identity (charts/ci-builders
	// `cacheRedirect`): one prefix of a bucket, read and written, and
	// nothing else. The service presigns its redirects with these same
	// credentials, so GetObject is also what every runner's download
	// through a redirect is authorized by.
	CacheRedirect struct {
		Estate

		RoleName       string
		PolicyName     string
		Description    string
		Namespace      string
		ServiceAccount string

		// Bucket is the cache bucket and Prefix the one key prefix (no
		// slashes) the service's front-end prefixes live under: its
		// `-nix-prefix` and `-goproxy-prefix` must both start with
		// "<Prefix>/".
		Bucket string
		Prefix string
	}
)

const (
	// RegistryShared is a pool whose registry authorization, base-image
	// pull-through and preview push come from a shared managed policy the
	// CALLER attaches to the role (its RoleName output). The pool's own
	// policy carries its runner images, the chart repositories and the
	// cache bucket.
	RegistryShared Registry = iota
	// RegistryOwn is a pool that restates registry authorization and a
	// read-only pull-through union, and may share the BuildKit warm cache,
	// with no push of any project's images.
	RegistryOwn
)

// WithArgs runs fn over the component's arguments just before it is
// registered. Constructors set everything they own first, so fn sees, and
// may adjust, the finished arguments.
func WithArgs(fn func(*podidentity.Args)) Option { return Option(fn) }

// WithNames names the component's children (the policy, the role, the
// attachment, the associations).
func WithNames(names podidentity.NameFunc) Option {
	return func(a *podidentity.Args) { a.Names = names }
}

// WithIgnoreAssociationTags ignores tag changes on the associations, for
// associations adopted with tags another tool set.
func WithIgnoreAssociationTags() Option {
	return func(a *podidentity.Args) { a.IgnoreAssociationTags = true }
}

// Statements returns the pool's policy statements, in order.
func (p Pool) Statements() ([]map[string]any, error) {
	if len(p.RunnerImageRepos) == 0 {
		return nil, errors.New("runneridentity: Pool.RunnerImageRepos is required")
	}

	if p.CacheBucket == "" {
		return nil, errors.New("runneridentity: Pool.CacheBucket is required")
	}

	switch p.Registry {
	case RegistryShared:
		if len(p.StableChartRepoARNs) == 0 {
			return nil, errors.New("runneridentity: Pool.StableChartRepoARNs is required with RegistryShared")
		}

		out := []statement{
			runnerImagesStatement(p.Estate, p.RunnerImageRepos),
			stableChartsReadStatement(p.StableChartRepoARNs),
		}

		return append(out, cacheStatements(p.CacheBucket)...), nil
	case RegistryOwn:
		if len(p.PullThroughRepos) == 0 {
			return nil, errors.New("runneridentity: Pool.PullThroughRepos is required with RegistryOwn")
		}

		out := pullThroughStatements(p.Estate, p.PullThroughRepos)
		out = append(out, runnerImagesStatement(p.Estate, p.RunnerImageRepos))
		out = append(out, cacheStatements(p.CacheBucket)...)

		if p.BuildkitCacheRepo != "" {
			out = append(out, buildkitCacheStatement(p.Estate, p.BuildkitCacheRepo))
		}

		return out, nil
	default:
		return nil, fmt.Errorf("runneridentity: unknown Pool.Registry %d", p.Registry)
	}
}

// Document is the pool's managed policy document.
func (p Pool) Document() (string, error) {
	statements, err := p.Statements()
	if err != nil {
		return "", err
	}

	return document(statements), nil
}

// NewPool registers the pool's pod-identity component under name.
func NewPool(ctx *pulumi.Context, name string, p Pool, opts ...Option) (*podidentity.PodIdentity, error) {
	doc, err := p.Document()
	if err != nil {
		return nil, err
	}

	return newPodIdentity(ctx, name, p.Estate, args{
		roleName: p.RoleName, namespace: p.Namespace, serviceAccount: p.ServiceAccount,
		policyName: p.PolicyName, description: p.Description, document: doc,
	}, opts)
}

// Document is the module proxy's managed policy document: list the prefix,
// read, write and delete objects under it.
func (m Modproxy) Document() (string, error) {
	if m.Bucket == "" || m.Prefix == "" {
		return "", errors.New("runneridentity: Modproxy.Bucket and Modproxy.Prefix are required")
	}

	return document([]statement{
		{
			polEffect:   polAllow,
			polAction:   s3ListBucket,
			polResource: s3ARN(m.Bucket),
			polCondition: map[string]any{
				polStringLik: map[string]any{"s3:prefix": m.Prefix + "/*"},
			},
		},
		{
			polEffect:   polAllow,
			polAction:   []string{s3GetObject, s3PutObject, s3DeleteObject},
			polResource: s3ARN(m.Bucket + "/" + m.Prefix + "/*"),
		},
	}), nil
}

// NewModproxy registers the module proxy's pod-identity component under
// name.
func NewModproxy(ctx *pulumi.Context, name string, m Modproxy, opts ...Option) (*podidentity.PodIdentity, error) {
	doc, err := m.Document()
	if err != nil {
		return nil, err
	}

	return newPodIdentity(ctx, name, m.Estate, args{
		roleName: m.RoleName, namespace: m.Namespace, serviceAccount: m.ServiceAccount,
		policyName: m.PolicyName, description: m.Description, document: doc,
	}, opts)
}

// Document is cache-redirect's managed policy document: list the prefix,
// so a missing key is a 404 (NoSuchKey) rather than a 403, which the
// service treats as an error and never as a miss; read objects under it,
// for the hits it serves and the URLs it presigns; write them, as one PUT
// or a multipart upload, and abort or inspect an upload that failed. No
// delete: objects expire by the bucket's lifecycle rules, never by the
// service.
func (c CacheRedirect) Document() (string, error) {
	if c.Bucket == "" || c.Prefix == "" {
		return "", errors.New("runneridentity: CacheRedirect.Bucket and CacheRedirect.Prefix are required")
	}

	return document([]statement{
		{
			polEffect:   polAllow,
			polAction:   s3ListBucket,
			polResource: s3ARN(c.Bucket),
			polCondition: map[string]any{
				polStringLik: map[string]any{"s3:prefix": c.Prefix + "/*"},
			},
		},
		{
			polEffect:   polAllow,
			polAction:   []string{s3GetObject, s3PutObject, s3AbortMultipartUpload, s3ListMultipartUploadParts},
			polResource: s3ARN(c.Bucket + "/" + c.Prefix + "/*"),
		},
	}), nil
}

// NewCacheRedirect registers cache-redirect's pod-identity component under
// name.
func NewCacheRedirect(ctx *pulumi.Context, name string, c CacheRedirect, opts ...Option) (*podidentity.PodIdentity, error) {
	doc, err := c.Document()
	if err != nil {
		return nil, err
	}

	return newPodIdentity(ctx, name, c.Estate, args{
		roleName: c.RoleName, namespace: c.Namespace, serviceAccount: c.ServiceAccount,
		policyName: c.PolicyName, description: c.Description, document: doc,
	}, opts)
}

// args is what one identity needs beyond the Estate.
type args struct {
	roleName, namespace, serviceAccount string
	policyName, description, document   string
}

func newPodIdentity(
	ctx *pulumi.Context, name string, e Estate, a args, opts []Option,
) (*podidentity.PodIdentity, error) {
	pa := &podidentity.Args{
		Provider:            e.Provider,
		ClusterName:         pulumi.String(e.Cluster),
		Region:              e.Region,
		Namespace:           a.namespace,
		ServiceAccounts:     []string{a.serviceAccount},
		RoleName:            a.roleName,
		PermissionsBoundary: e.BoundaryARN,
		AccountID:           e.AccountID,
		ClusterARN:          fmt.Sprintf("arn:%s:eks:%s:%s:cluster/%s", partition, e.Region, e.AccountID, e.Cluster),
		ManagedPolicy: &podidentity.ManagedPolicy{
			Name:        a.policyName,
			Description: a.description,
			Document:    pulumi.String(a.document),
		},
	}

	for _, opt := range opts {
		opt(pa)
	}

	return podidentity.New(ctx, name, pa)
}
