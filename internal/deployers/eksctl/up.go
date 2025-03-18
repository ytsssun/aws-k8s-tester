package eksctl

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/aws/aws-k8s-tester/internal/util"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	ekstypes "github.com/aws/aws-sdk-go-v2/service/eks/types"
	"k8s.io/klog"
)

type UpOptions struct {
	Region               string   `flag:"region" desc:"AWS region for EKS cluster"`
	KubernetesVersion    string   `flag:"kubernetes-version" desc:"cluster Kubernetes version"`
	Nodes                int      `flag:"nodes" desc:"number of nodes to launch in cluster"`
	AMI                  string   `flag:"ami" desc:"Node AMI"`
	InstanceTypes        []string `flag:"instance-types" desc:"Node instance types"`
	ConfigFile           string   `flag:"config-file" desc:"Path to eksctl config file (if provided, other flags are ignored)"`
	AvailabilityZones    []string `flag:"availability-zones" desc:"Node availability zones"`
	AMIFamily            string   `flag:"ami-family" desc:"AMI family to use (AmazonLinux2, Bottlerocket)"`
	EFAEnabled           bool     `flag:"efa-enabled" desc:"Enable Elastic Fabric Adapter for the nodegroup"`
	VolumeSize           int      `flag:"volume-size" desc:"Size of the node root volume in GB"`
	PrivateNetworking    bool     `flag:"private-networking" desc:"Use private networking for nodes"`
	WithOIDC             bool     `flag:"with-oidc" desc:"Enable OIDC provider for IAM roles for service accounts"`
	SkipClusterCreation  bool     `flag:"skip-cluster-creation" desc:"Skip cluster creation, only create nodegroups"`
	ClusterName          string   `flag:"cluster-name" desc:"Name of the EKS cluster (defaults to RunID if not specified)"`
	UseUnmanagedNodegroup bool    `flag:"unmanaged-nodegroup" desc:"Use unmanaged nodegroup instead of managed nodegroup"`
	NodegroupName        string   `flag:"nodegroup-name" desc:"Name of the nodegroup (defaults to 'ng-1' for unmanaged or 'managed' for managed nodegroups)"`
}

func (d *deployer) verifyUpFlags() error {
	if d.UpOptions.KubernetesVersion == "" {
			klog.Infof("--kubernetes-version is empty, attempting to detect it...")
			detectedVersion, err := detectKubernetesVersion()
			if err != nil {
					return fmt.Errorf("unable to detect --kubernetes-version, flag cannot be empty")
			}
			klog.Infof("detected --kubernetes-version=%s", detectedVersion)
			d.UpOptions.KubernetesVersion = detectedVersion
	}
	if d.UpOptions.Nodes <= 0 {
			return fmt.Errorf("number of nodes must be greater than zero")
	}
	
	// If Bottlerocket AMI family is specified with a custom AMI ID, 
	// ensure we use unmanaged nodegroups as managed nodegroups don't support this combination
	if d.UpOptions.AMIFamily == "Bottlerocket" && d.UpOptions.AMI != "" && !d.UpOptions.UseUnmanagedNodegroup {
			klog.Warningf("Bottlerocket with custom AMI requires unmanaged nodegroups. Setting --unmanaged-nodegroup=true")
			d.UpOptions.UseUnmanagedNodegroup = true
	}
	
	// Validate instance types for unmanaged nodegroups
	if d.UpOptions.UseUnmanagedNodegroup {
		if len(d.UpOptions.InstanceTypes) > 1 {
				return fmt.Errorf("Unmanaged nodegroups only support a single instance type. Using the first one: %s", d.UpOptions.InstanceTypes[0])
		} else if len(d.UpOptions.InstanceTypes) == 0 {
				// If no instance type specified, use a default
				d.UpOptions.InstanceTypes = []string{"m5.xlarge"}
				return fmt.Errorf("No instance type specified for unmanaged nodegroup. Using default: %s", d.UpOptions.InstanceTypes[0])
		}
	}

	return nil
}

func (d *deployer) Up() error {
	d.initClusterName()
	
	if err := d.verifyUpFlags(); err != nil {
			return fmt.Errorf("up flags are invalid: %v", err)
	}
	
	if d.UpOptions.UseUnmanagedNodegroup {
		klog.Infof("Using unmanaged nodegroup for cluster %s", d.clusterName)
	} else {
		klog.Infof("Using managed nodegroup for cluster %s", d.clusterName)
	}

	var args []string
	
	if d.ConfigFile != "" {
			// If config file is provided, use it
			if d.SkipClusterCreation {
					klog.Infof("Adding nodegroup to existing cluster %s using config file: %s", d.clusterName, d.ConfigFile)
					args = []string{
							"create",
							"nodegroup",
							"--config-file", d.ConfigFile,
					}
			} else {
					klog.Infof("Creating cluster with config file: %s", d.ConfigFile)
					args = []string{
							"create",
							"cluster",
							"--config-file", d.ConfigFile,
					}
			}
	} else {
			// Use rendered cluster config
			clusterConfig, err := d.RenderClusterConfig()
			if err != nil {
					return err
			}
			klog.Infof("Rendered cluster config: %s", string(clusterConfig))
			
			clusterConfigFile, err := os.CreateTemp("", "kubetest2-eksctl-cluster-config")
			if err != nil {
					return err
			}
			defer clusterConfigFile.Close()
			
			_, err = clusterConfigFile.Write(clusterConfig)
			if err != nil {
					return err
			}
			
			if d.SkipClusterCreation {
					klog.Infof("Adding nodegroup to existing cluster %s", d.clusterName)
					args = []string{
							"create",
							"nodegroup",
							"--config-file", clusterConfigFile.Name(),
					}
			} else {
					klog.Infof("Creating cluster: %s", d.clusterName)
					args = []string{
							"create",
							"cluster",
							"--config-file", clusterConfigFile.Name(),
					}
			}
	}
	
	err := util.ExecuteCommand("eksctl", args...)
	if err != nil {
			return fmt.Errorf("failed to create cluster: %v", err)
	}

	// Write kubeconfig to the rundir
	kubeConfigPath, err := d.Kubeconfig()
	if err != nil {
			return fmt.Errorf("error determining kubeconfig path: %v", err)
	}
	
	// Create directory if it doesn't exist
	err = os.MkdirAll(filepath.Dir(kubeConfigPath), 0755)
	if err != nil {
			return fmt.Errorf("error creating directory for kubeconfig: %v", err)
	}
	
	klog.Infof("Writing kubeconfig to %s", kubeConfigPath)
	writeKubeconfigArgs := []string{
			"utils",
			"write-kubeconfig",
			"--cluster", d.clusterName,
			"--region", d.UpOptions.Region,
			"--kubeconfig", kubeConfigPath,
	}
	
	err = util.ExecuteCommand("eksctl", writeKubeconfigArgs...)
	if err != nil {
			return fmt.Errorf("failed to write kubeconfig: %v", err)
	}
	
	klog.Infof("Successfully wrote kubeconfig to %s", kubeConfigPath)
	d.KubeconfigPath = kubeConfigPath
	return nil
}

func (d *deployer) IsUp() (up bool, err error) {
	d.initClusterName()
	
	result, err := d.eksClient.DescribeCluster(context.TODO(), &eks.DescribeClusterInput{
			Name: aws.String(d.clusterName),
	})
	if err != nil {
			return false, err
	}
	switch result.Cluster.Status {
	case ekstypes.ClusterStatusActive:
			return true, nil
	case ekstypes.ClusterStatusCreating:
			return false, nil
	default:
			return false, fmt.Errorf("cluster status is: %v", result.Cluster.Status)
	}
}

func detectKubernetesVersion() (string, error) {
	detectedVersion, err := util.DetectKubernetesVersion()
	if err != nil {
		return "", err
	}
	minorVersion, err := util.ParseMinorVersion(detectedVersion)
	if err != nil {
		return "", err
	}
	return minorVersion, nil
}
