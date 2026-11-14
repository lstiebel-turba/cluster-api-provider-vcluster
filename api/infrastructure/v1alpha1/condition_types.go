package v1alpha1

// Condition types of a VCluster, following the Cluster API v1beta2 contract.
const (
	// ReadyCondition summarizes the VCluster as an infrastructure cluster. Cluster API mirrors it
	// into the Cluster's InfrastructureReady condition.
	ReadyCondition = "Ready"

	// AvailableCondition summarizes the VCluster as a control plane. Cluster API mirrors it into
	// the Cluster's ControlPlaneAvailable condition.
	AvailableCondition = "Available"

	// PausedCondition is true while the owning Cluster or the VCluster is paused.
	PausedCondition = "Paused"

	// ControlPlaneInitializedCondition is true once the vcluster API server answered.
	ControlPlaneInitializedCondition = "ControlPlaneInitialized"

	// KubeconfigReadyCondition is true once the <cluster>-kubeconfig Secret is written.
	KubeconfigReadyCondition = "KubeconfigReady"

	// HelmChartDeployedCondition is true once the vcluster chart is installed or upgraded.
	HelmChartDeployedCondition = "HelmChartDeployed"

	// InfrastructureClusterSyncedCondition is true once another infrastructure cluster got the
	// control plane endpoint.
	InfrastructureClusterSyncedCondition = "InfrastructureClusterSynced"
)

// Condition reasons of a VCluster.
const (
	// HelmDeployFailedReason means the chart install or upgrade failed.
	HelmDeployFailedReason = "HelmDeployFailed"

	// PatchFailedReason means the infrastructure cluster could not be patched.
	PatchFailedReason = "PatchFailed"

	// CheckFailedReason means the vcluster API server could not be reached yet.
	CheckFailedReason = "CheckFailed"

	// SucceededReason is the reason of a condition that is true.
	SucceededReason = "Succeeded"

	// NotReadyReason means the vcluster is not ready yet.
	NotReadyReason = "NotReady"
)
