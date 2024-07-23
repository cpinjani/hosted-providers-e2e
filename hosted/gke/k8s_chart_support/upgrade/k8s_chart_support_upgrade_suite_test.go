package k8s_chart_support_upgrade_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/rancher-sandbox/ele-testhelpers/kubectl"
	"github.com/rancher-sandbox/ele-testhelpers/tools"
	. "github.com/rancher-sandbox/qase-ginkgo"
	"github.com/rancher/shepherd/clients/rancher"
	management "github.com/rancher/shepherd/clients/rancher/generated/management/v3"
	"github.com/rancher/shepherd/extensions/clusters"
	nodestat "github.com/rancher/shepherd/extensions/nodes"
	"github.com/rancher/shepherd/extensions/pipeline"
	"github.com/rancher/shepherd/extensions/workloads/pods"
	"github.com/rancher/shepherd/pkg/config"
	namegen "github.com/rancher/shepherd/pkg/namegenerator"

	"github.com/rancher/hosted-providers-e2e/hosted/gke/helper"
	"github.com/rancher/hosted-providers-e2e/hosted/helpers"
)

var (
	ctx                     helpers.Context
	clusterName, k8sVersion string
	testCaseID              int64
	zone                    = helpers.GetGKEZone()
	project                 = helpers.GetGKEProjectID()
)

func TestK8sChartSupportUpgrade(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "K8sChartSupportUpgrade Suite")
}

var _ = SynchronizedBeforeSuite(func() []byte {
	helpers.CommonSynchronizedBeforeSuite()
	return nil
}, func() {
	Expect(helpers.K8sUpgradedMinorVersion).ToNot(BeEmpty())
	Expect(helpers.Kubeconfig).ToNot(BeEmpty())

	By("Adding the necessary chart repos", func() {
		helpers.AddRancherCharts()
	})

	By(fmt.Sprintf("Installing Rancher Manager v%s", helpers.RancherVersion), func() {
		helpers.DeployRancherManager(helpers.RancherVersion, true)
	})

	ctx = helpers.CommonBeforeSuite()
})

var _ = BeforeEach(func() {
	var err error
	clusterName = namegen.AppendRandomString(helpers.ClusterNamePrefix)

	// For k8s chart support upgrade we want to begin with the default k8s version; we will upgrade rancher and then upgrade k8s to the default available there.
	k8sVersion, err = helper.GetK8sVersion(ctx.RancherAdminClient, project, ctx.CloudCred.ID, zone, "", false)
	Expect(err).To(BeNil())
	GinkgoLogr.Info(fmt.Sprintf("Using GKE version %s for cluster %s", k8sVersion, clusterName))
})

var _ = AfterEach(func() {
	// The test must restore the env to its original state, so we install rancher back to its original version and uninstall the operator charts
	By(fmt.Sprintf("Installing Rancher back to its original version %s", helpers.RancherVersion), func() {
		helpers.DeployRancherManager(helpers.RancherVersion, true)
	})

	By("Uninstalling the existing operator charts", func() {
		helpers.UninstallOperatorCharts()
	})
})

var _ = ReportBeforeEach(func(report SpecReport) {
	// Reset case ID
	testCaseID = -1
})

var _ = ReportAfterEach(func(report SpecReport) {
	// Add result in Qase if asked
	Qase(testCaseID, report)
})

// commonChartSupportUpgrade runs the common checks required for testing chart support
func commonChartSupportUpgrade(ctx *helpers.Context, cluster *management.Cluster, clusterName, rancherUpgradedVersion, hostname, k8sUpgradedVersion string) {
	helpers.ClusterIsReadyChecks(cluster, ctx.RancherAdminClient, clusterName)

	var originalChartVersion string
	By("checking the chart version", func() {
		originalChartVersion = helpers.GetCurrentOperatorChartVersion()
		Expect(originalChartVersion).ToNot(BeEmpty())
		GinkgoLogr.Info("Original chart version: " + originalChartVersion)
	})

	By("upgrading rancher", func() {
		helpers.DeployRancherManager(rancherUpgradedVersion, true)

		By("ensuring operator pods are also up", func() {
			Eventually(func() error {
				return kubectl.New().WaitForNamespaceWithPod(helpers.CattleSystemNS, fmt.Sprintf("ke.cattle.io/operator=%s", helpers.Provider))
			}, tools.SetTimeout(4*time.Minute), 30*time.Second).Should(BeNil())
		})

		By("regenerating the token and initiating a new rancher client", func() {
			//	regenerate the tokens and initiate a new rancher client
			rancherConfig := new(rancher.Config)
			config.LoadConfig(rancher.ConfigurationFileKey, rancherConfig)

			token, err := pipeline.CreateAdminToken(helpers.RancherPassword, rancherConfig)
			Expect(err).To(BeNil())

			config.LoadAndUpdateConfig(rancher.ConfigurationFileKey, rancherConfig, func() {
				rancherConfig.AdminToken = token
			})
			rancherAdminClient, err := rancher.NewClient(rancherConfig.AdminToken, ctx.Session)
			Expect(err).To(BeNil())
			ctx.RancherAdminClient = rancherAdminClient

			setting := new(management.Setting)
			resp, err := rancherAdminClient.Management.Setting.ByID("server-url")
			Expect(err).To(BeNil())

			setting.Source = "env"
			setting.Value = fmt.Sprintf("https://%s", hostname)
			resp, err = rancherAdminClient.Management.Setting.Update(resp, setting)
			Expect(err).To(BeNil())

			var isConnected bool
			isConnected, err = ctx.RancherAdminClient.IsConnected()
			Expect(err).To(BeNil())
			Expect(isConnected).To(BeTrue())
		})
	})

	By("making sure the local cluster is ready", func() {
		localClusterID := "local"
		By("checking all management nodes are ready", func() {
			err := nodestat.AllManagementNodeReady(ctx.RancherAdminClient, localClusterID, helpers.Timeout)
			Expect(err).To(BeNil())
		})

		By("checking all pods are ready", func() {
			podErrors := pods.StatusPods(ctx.RancherAdminClient, localClusterID)
			Expect(podErrors).To(BeEmpty())
		})
	})

	var upgradedChartVersion string

	By("checking the chart version and validating it is > the old version", func() {
		// the chart is sometimes auto-upgraded to the latest version (mostly happens when running the test on un-rc-ed charts, so we check with `>=`
		helpers.WaitUntilOperatorChartInstallation(originalChartVersion, ">=", 0)
		upgradedChartVersion = helpers.GetCurrentOperatorChartVersion()
		GinkgoLogr.Info("Upgraded chart version: " + upgradedChartVersion)

	})

	By(fmt.Sprintf("fetching a list of available k8s versions and ensuring v%s is present in the list and upgrading the cluster to it", k8sUpgradedVersion), func() {
		versions, err := helper.ListGKEAvailableVersions(ctx.RancherAdminClient, cluster.ID)
		Expect(err).To(BeNil())
		highestSupportedVersionByUI := helpers.HighestK8sMinorVersionSupportedByUI(ctx.RancherAdminClient)
		var latestVersion string
		for _, v := range versions {
			if strings.Contains(v, highestSupportedVersionByUI) {
				latestVersion = v
			}
		}
		Expect(latestVersion).To(ContainSubstring(k8sUpgradedVersion))
		Expect(helpers.VersionCompare(latestVersion, cluster.Version.GitVersion)).To(BeNumerically("==", 1))

		cluster, err = helper.UpgradeKubernetesVersion(cluster, latestVersion, ctx.RancherAdminClient, true, true, true)
		Expect(err).To(BeNil())
	})

	var downgradeVersion string
	By("fetching a value to downgrade to", func() {
		downgradeVersion = helpers.GetDowngradeOperatorChartVersion(upgradedChartVersion)
	})

	By("downgrading the chart version", func() {
		helpers.DowngradeProviderChart(downgradeVersion)
	})

	By("making a change to the cluster to validate functionality after chart downgrade", func() {
		initialNodeCount := *cluster.GKEConfig.NodePools[0].InitialNodeCount
		var err error
		cluster, err = helper.ScaleNodePool(cluster, ctx.RancherAdminClient, initialNodeCount+1, true, true)
		Expect(err).To(BeNil())
	})

	By("uninstalling the operator chart", func() {
		helpers.UninstallOperatorCharts()
	})

	By("making a change(adding a nodepool) to the cluster to re-install the operator and validating it is re-installed to the latest/upgraded version", func() {
		currentNodePoolNumber := len(cluster.GKEConfig.NodePools)
		var err error
		cluster, err = helper.AddNodePool(cluster, ctx.RancherAdminClient, 1, "", false, false)
		Expect(err).To(BeNil())

		Expect(len(cluster.GKEConfig.NodePools)).To(BeNumerically("==", currentNodePoolNumber+1))

		By("ensuring that the chart is re-installed to the latest/upgraded version", func() {
			helpers.WaitUntilOperatorChartInstallation(upgradedChartVersion, "", 0)
		})

		err = clusters.WaitClusterToBeUpgraded(ctx.RancherAdminClient, cluster.ID)
		Expect(err).To(BeNil())

		Eventually(func() int {
			GinkgoLogr.Info("Waiting for the total nodepool count to increase in GKEStatus.UpstreamSpec ...")
			cluster, err = ctx.RancherAdminClient.Management.Cluster.ByID(cluster.ID)
			Expect(err).To(BeNil())
			return len(cluster.GKEStatus.UpstreamSpec.NodePools)
		}, tools.SetTimeout(12*time.Minute), 10*time.Second).Should(BeNumerically("==", currentNodePoolNumber+1))

	})

}
