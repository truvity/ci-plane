// Package runneridentity is the AWS side of the CI plane's runner pods: the
// Pod Identity roles the ARC runner pools, the Go module proxy and
// cache-redirect assume.
//
// The chart half of the CI plane (charts/arc-runners, charts/ci-builders)
// names a ServiceAccount for every pod that talks to AWS and nothing more;
// what that ServiceAccount may DO is IAM, which a chart cannot own. This
// package is that policy, as Pulumi: the statements a runner pool needs
// (pull its images, read through the registry's pull-through caches, use the
// disposable CI cache bucket), the one-prefix grants of the module proxy and
// of cache-redirect, and the pod-identity component that binds each role to
// its ServiceAccount.
//
// What it will not do is deploy-shaped authority. Cluster access and stable
// publishing never belong to a pool's identity: a workflow step that forgets
// its own OIDC login must still hit a loud AccessDenied on anything beyond
// the dev loop. Nothing here grants them and no input can.
//
// Every name an estate chose (roles, policies, namespaces, ServiceAccounts,
// bucket, repositories) is an input. Resources are created through
// github.com/truvity/k8s/pkg/aws/podidentity, and the caller shapes them
// with functional options (WithArgs and the helpers below) so an estate that
// already runs these roles keeps their logical names and URNs.
package runneridentity
