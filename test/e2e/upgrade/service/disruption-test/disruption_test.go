package main

import (
	"context"
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/test/e2e/framework"
	"k8s.io/kubernetes/test/e2e/upgrades"
	"k8s.io/kubernetes/test/utils/junit"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/openshift/origin/test/e2e/upgrade/service"
	exutil "github.com/openshift/origin/test/extended/util"
)

var ctx = context.Background()

func TestMain(t *testing.T) {
	framework.TestContext.KubeConfig = exutil.KubeConfigPath()
	framework.TestContext.CloudConfig.Provider = framework.NullProvider{}
	framework.NewFrameworkExtensions = nil

	RegisterFailHandler(Fail)
	RunSpecs(t, "Disruption tests")
}

var _ = Describe("Disruption tests", func() {
	cli := exutil.NewCLIWithoutNamespace("diruption-test").AsAdmin()

	upgradeContext := &upgrades.UpgradeContext{}
	tests := []upgrades.Test{
		service.NewServiceLoadBalancerWithNewConnectionsTest(),
		service.NewServiceLoadBalancerWithReusedConnectionsTest(),
	}

	frameworks := upgrades.CreateUpgradeFrameworks(tests)
	testSuite := &junit.TestSuite{Name: "disruption tests"}

	Expect(exutil.InitTest(false)).To(Succeed())

	BeforeEach(func() {
		exutil.WithCleanup(func() {})
	})

	FIt("should not disrupt connections when nodes are rebooted", func() {
		// Run the test.
		upgradeFunc := func() {
			nodes, err := cli.KubeClient().CoreV1().Nodes().List(ctx, metav1.ListOptions{})
			Expect(err).ToNot(HaveOccurred())

			Expect(nodes.Items).ToNot(BeEmpty())

			for _, node := range nodes.Items {
				Expect(drainNode(ctx, cli, node)).To(Succeed())
				Expect(rebootNode(ctx, cli, node)).To(Succeed())
				Expect(uncordonNode(ctx, cli, node)).To(Succeed())
			}
		}

		upgrades.RunUpgradeSuite(
			upgradeContext,
			tests,
			frameworks,
			testSuite,
			upgrades.NodeUpgrade,
			upgradeFunc,
		)
	})
})

func drainNode(ctx context.Context, cli *exutil.CLI, node corev1.Node) error {
	Expect(node.Status.Addresses).ToNot(BeEmpty())

	klog.Infof("Draining node %s (%s)", node.Name, node.Status.Addresses[0].Address)

	drainLog, err := cli.Run("adm", "drain", node.Name, "--force", "--ignore-daemonsets", "--delete-local-data", "--timeout=10m").Output()
	if err != nil {
		return fmt.Errorf("failed to drain node %s: %w", node.Name, err)
	}

	klog.Infof("Drain log: %s", drainLog)

	return nil
}

func uncordonNode(ctx context.Context, cli *exutil.CLI, node corev1.Node) error {
	Expect(node.Status.Addresses).ToNot(BeEmpty())

	klog.Infof("Uncordoning node %s (%s)", node.Name, node.Status.Addresses[0].Address)

	uncordonLog, err := cli.Run("adm", "uncordon", node.Name).Output()
	if err != nil {
		return fmt.Errorf("failed to uncordon node %s: %w", node.Name, err)
	}

	klog.Infof("Uncordon log: %s", uncordonLog)

	return nil
}

func rebootNode(ctx context.Context, cli *exutil.CLI, node corev1.Node) error {
	Expect(node.Status.Addresses).ToNot(BeEmpty())

	klog.Infof("Rebooting node %s (%s)", node.Name, node.Status.Addresses[0].Address)

	rebootLog, err := cli.Run("debug", fmt.Sprintf("node/%s", node.Name), "--tty", "--", "chroot", "/host", "shutdown", "-r").Output()
	if err != nil {
		return fmt.Errorf("failed to reboot node %s: %w", node.Name, err)
	}

	// Expect the node to reboot about a minute after this command is run.
	rebootTime := time.Now().Add(1 * time.Minute)

	klog.Infof("Reboot log: %s", rebootLog)

	// Eventually the node should go unready, this is how we know it's rebooted.
	Eventually(func() (*corev1.Node, error) {
		return cli.KubeClient().CoreV1().Nodes().Get(ctx, node.Name, metav1.GetOptions{})
	}, 5*time.Minute).Should(HaveField("Status.Conditions", ContainElement(SatisfyAll(
		HaveField("Type", Equal(corev1.NodeReady)),
		SatisfyAny(
			HaveField("Status", Equal(corev1.ConditionFalse)),                   // If we see the node as false, we know it is down.
			HaveField("LastTransitionTime.Time", BeTemporally(">", rebootTime)), // If we see the node as true, but the last transition time is after the reboot time, we know it went down at some point. We missed it flicking unready.
		),
	))), "Node should go unready during a reboot")

	klog.Infof("Node %s is unready", node.Name)

	// Once it's gone unready, we need to wait for it to be ready again.
	Eventually(func() (*corev1.Node, error) {
		return cli.KubeClient().CoreV1().Nodes().Get(ctx, node.Name, metav1.GetOptions{})
	}, 5*time.Minute).Should(HaveField("Status.Conditions", ContainElement(SatisfyAll(
		HaveField("Type", Equal(corev1.NodeReady)),
		HaveField("Status", Equal(corev1.ConditionTrue)),
	))), "Node should become ready after a reboot")

	klog.Infof("Node %s is ready", node.Name)

	return nil
}
