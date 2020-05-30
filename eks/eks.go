// Package eks implements EKS cluster operations.
// It implements "k8s.io/test-infra/kubetest2/pkg/types.Deployer" and
// "k8s.io/test-infra/kubetest2/pkg/types.Options".
// ref. https://pkg.go.dev/k8s.io/test-infra/kubetest2/pkg/types?tab=doc#Deployer
// ref. https://pkg.go.dev/k8s.io/test-infra/kubetest2/pkg/types?tab=doc#Options
package eks

import (
	"context"
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/aws/aws-k8s-tester/ec2config"
	alb_2048 "github.com/aws/aws-k8s-tester/eks/alb-2048"
	app_mesh "github.com/aws/aws-k8s-tester/eks/app-mesh"
	cluster_loader_local "github.com/aws/aws-k8s-tester/eks/cluster-loader/local"
	cluster_loader_remote "github.com/aws/aws-k8s-tester/eks/cluster-loader/remote"
	config_maps_local "github.com/aws/aws-k8s-tester/eks/config-maps/local"
	config_maps_remote "github.com/aws/aws-k8s-tester/eks/config-maps/remote"
	"github.com/aws/aws-k8s-tester/eks/conformance"
	cron_jobs "github.com/aws/aws-k8s-tester/eks/cron-jobs"
	csi_ebs "github.com/aws/aws-k8s-tester/eks/csi-ebs"
	csrs_local "github.com/aws/aws-k8s-tester/eks/csrs/local"
	csrs_remote "github.com/aws/aws-k8s-tester/eks/csrs/remote"
	"github.com/aws/aws-k8s-tester/eks/fargate"
	"github.com/aws/aws-k8s-tester/eks/gpu"
	hollow_nodes_local "github.com/aws/aws-k8s-tester/eks/hollow-nodes/local"
	hollow_nodes_remote "github.com/aws/aws-k8s-tester/eks/hollow-nodes/remote"
	"github.com/aws/aws-k8s-tester/eks/irsa"
	irsa_fargate "github.com/aws/aws-k8s-tester/eks/irsa-fargate"
	jobs_echo "github.com/aws/aws-k8s-tester/eks/jobs-echo"
	jobs_pi "github.com/aws/aws-k8s-tester/eks/jobs-pi"
	jupyter_hub "github.com/aws/aws-k8s-tester/eks/jupyter-hub"
	"github.com/aws/aws-k8s-tester/eks/kubeflow"
	kubernetes_dashboard "github.com/aws/aws-k8s-tester/eks/kubernetes-dashboard"
	"github.com/aws/aws-k8s-tester/eks/mng"
	"github.com/aws/aws-k8s-tester/eks/ng"
	nlb_hello_world "github.com/aws/aws-k8s-tester/eks/nlb-hello-world"
	prometheus_grafana "github.com/aws/aws-k8s-tester/eks/prometheus-grafana"
	secrets_local "github.com/aws/aws-k8s-tester/eks/secrets/local"
	secrets_remote "github.com/aws/aws-k8s-tester/eks/secrets/remote"
	stresser_local "github.com/aws/aws-k8s-tester/eks/stresser/local"
	stresser_remote "github.com/aws/aws-k8s-tester/eks/stresser/remote"
	"github.com/aws/aws-k8s-tester/eks/wordpress"
	"github.com/aws/aws-k8s-tester/eksconfig"
	pkg_aws "github.com/aws/aws-k8s-tester/pkg/aws"
	"github.com/aws/aws-k8s-tester/pkg/fileutil"
	"github.com/aws/aws-k8s-tester/pkg/httputil"
	k8s_client "github.com/aws/aws-k8s-tester/pkg/k8s-client"
	"github.com/aws/aws-k8s-tester/pkg/logutil"
	"github.com/aws/aws-k8s-tester/version"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/autoscaling"
	"github.com/aws/aws-sdk-go/service/autoscaling/autoscalingiface"
	"github.com/aws/aws-sdk-go/service/cloudformation"
	"github.com/aws/aws-sdk-go/service/cloudformation/cloudformationiface"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/ec2/ec2iface"
	"github.com/aws/aws-sdk-go/service/ecr"
	"github.com/aws/aws-sdk-go/service/ecr/ecriface"
	aws_eks "github.com/aws/aws-sdk-go/service/eks"
	"github.com/aws/aws-sdk-go/service/eks/eksiface"
	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/aws/aws-sdk-go/service/elbv2/elbv2iface"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/aws/aws-sdk-go/service/iam/iamiface"
	"github.com/aws/aws-sdk-go/service/kms"
	"github.com/aws/aws-sdk-go/service/kms/kmsiface"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/service/s3/s3iface"
	"github.com/aws/aws-sdk-go/service/ssm"
	"github.com/aws/aws-sdk-go/service/ssm/ssmiface"
	"github.com/aws/aws-sdk-go/service/sts"
	"github.com/dustin/go-humanize"
	"go.uber.org/zap"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/exec"
)

// Tester implements "kubetest2" Deployer.
// ref. https://pkg.go.dev/k8s.io/test-infra/kubetest2/pkg/types?tab=doc
type Tester struct {
	stopCreationCh     chan struct{}
	stopCreationChOnce *sync.Once

	osSig chan os.Signal

	downMu *sync.Mutex

	lg  *zap.Logger
	cfg *eksconfig.Config

	awsSession *session.Session
	iamAPI     iamiface.IAMAPI
	kmsAPI     kmsiface.KMSAPI
	ssmAPI     ssmiface.SSMAPI
	cfnAPI     cloudformationiface.CloudFormationAPI
	ec2API     ec2iface.EC2API
	s3API      s3iface.S3API
	asgAPI     autoscalingiface.AutoScalingAPI
	elbv2API   elbv2iface.ELBV2API
	ecrAPI     ecriface.ECRAPI

	// used for EKS + EKS MNG API calls
	eksSession *session.Session
	eksAPI     eksiface.EKSAPI

	k8sClient k8s_client.EKS

	ngTester  ng.Tester
	mngTester mng.Tester
	gpuTester gpu.Tester

	conformanceTester conformance.Tester

	csiEBSTester  csi_ebs.Tester
	appMeshTester app_mesh.Tester

	kubernetesDashboardTester kubernetes_dashboard.Tester
	prometheusGrafanaTester   prometheus_grafana.Tester
	nlbHelloWorldTester       nlb_hello_world.Tester
	alb2048Tester             alb_2048.Tester
	jobsPiTester              jobs_pi.Tester
	jobsEchoTester            jobs_echo.Tester
	cronJobsTester            cron_jobs.Tester

	csrsLocalTester  csrs_local.Tester
	csrsRemoteTester csrs_remote.Tester

	configMapsLocalTester  config_maps_local.Tester
	configMapsRemoteTester config_maps_remote.Tester

	secretsLocalTester  secrets_local.Tester
	secretsRemoteTester secrets_remote.Tester

	fargateTester     fargate.Tester
	irsaTester        irsa.Tester
	irsaFargateTester irsa_fargate.Tester
	wordPressTester   wordpress.Tester
	jupyterHubTester  jupyter_hub.Tester
	kubeflowTester    kubeflow.Tester

	clusterLoaderLocalTester  cluster_loader_local.Tester
	clusterLoaderRemoteTester cluster_loader_remote.Tester

	hollowNodesLocalTester  hollow_nodes_local.Tester
	hollowNodesRemoteTester hollow_nodes_remote.Tester

	stresserLocalTester  stresser_local.Tester
	stresserRemoteTester stresser_remote.Tester
}

// New returns a new EKS kubetest2 Deployer.
// ref. https://pkg.go.dev/k8s.io/test-infra/kubetest2/pkg/types?tab=doc#Deployer
// ref. https://pkg.go.dev/k8s.io/test-infra/kubetest2/pkg/types?tab=doc#Options
func New(cfg *eksconfig.Config) (ts *Tester, err error) {
	fmt.Println("😎 🙏 🚶 ✔️ 👍")
	fmt.Println(version.Version())
	fmt.Printf("\n*********************************\n")
	fmt.Printf("New %q\n", cfg.ConfigPath)
	if err := cfg.ValidateAndSetDefaults(); err != nil {
		return nil, err
	}

	lcfg := logutil.AddOutputPaths(logutil.GetDefaultZapLoggerConfig(), cfg.LogOutputs, cfg.LogOutputs)
	lcfg.Level = zap.NewAtomicLevelAt(logutil.ConvertToZapLevel(cfg.LogLevel))
	var lg *zap.Logger
	lg, err = lcfg.Build()
	if err != nil {
		return nil, err
	}

	if err = fileutil.EnsureExecutable(cfg.AWSCLIPath); err != nil {
		// file may be already executable while the process does not own the file/directory
		// ref. https://github.com/aws/aws-k8s-tester/issues/66
		lg.Warn("failed to ensure executable", zap.Error(err))
		err = nil
	}

	// aws --version
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	var vo []byte
	vo, err = exec.New().CommandContext(ctx, cfg.AWSCLIPath, "--version").CombinedOutput()
	cancel()
	if err != nil {
		return nil, fmt.Errorf("'aws --version' failed (output %q, error %v)", string(vo), err)
	}
	lg.Info(
		"aws version",
		zap.String("aws-cli-path", cfg.AWSCLIPath),
		zap.String("aws-version", string(vo)),
	)

	lg.Info("mkdir", zap.String("kubectl-path-dir", filepath.Dir(cfg.KubectlPath)))
	if err = os.MkdirAll(filepath.Dir(cfg.KubectlPath), 0700); err != nil {
		return nil, fmt.Errorf("could not create %q (%v)", filepath.Dir(cfg.KubectlPath), err)
	}
	if !fileutil.Exist(cfg.KubectlPath) {
		if cfg.KubectlDownloadURL == "" {
			return nil, fmt.Errorf("%q does not exist but no download URL", cfg.KubectlPath)
		}
		cfg.KubectlPath, _ = filepath.Abs(cfg.KubectlPath)
		lg.Info("downloading kubectl", zap.String("kubectl-path", cfg.KubectlPath))
		if err = httputil.Download(lg, os.Stderr, cfg.KubectlDownloadURL, cfg.KubectlPath); err != nil {
			return nil, err
		}
	} else {
		lg.Info("skipping kubectl download; already exist", zap.String("kubectl-path", cfg.KubectlPath))
	}
	if err = fileutil.EnsureExecutable(cfg.KubectlPath); err != nil {
		// file may be already executable while the process does not own the file/directory
		// ref. https://github.com/aws/aws-k8s-tester/issues/66
		lg.Warn("failed to ensure executable", zap.Error(err))
		err = nil
	}
	// kubectl version --client=true
	ctx, cancel = context.WithTimeout(context.Background(), 15*time.Second)
	vo, err = exec.New().CommandContext(ctx, cfg.KubectlPath, "version", "--client=true").CombinedOutput()
	cancel()
	if err != nil {
		return nil, fmt.Errorf("'kubectl version' failed (output %q, error %v)", string(vo), err)
	}
	lg.Info(
		"kubectl version",
		zap.String("kubectl-path", cfg.KubectlPath),
		zap.String("kubectl-version", string(vo)),
	)

	if cfg.AWSIAMAuthenticatorPath != "" && cfg.AWSIAMAuthenticatorDownloadURL != "" {
		lg.Info("mkdir", zap.String("aws-iam-authenticator-path-dir", filepath.Dir(cfg.AWSIAMAuthenticatorPath)))
		if err = os.MkdirAll(filepath.Dir(cfg.AWSIAMAuthenticatorPath), 0700); err != nil {
			return nil, fmt.Errorf("could not create %q (%v)", filepath.Dir(cfg.AWSIAMAuthenticatorPath), err)
		}
		if !fileutil.Exist(cfg.AWSIAMAuthenticatorPath) {
			cfg.AWSIAMAuthenticatorPath, _ = filepath.Abs(cfg.AWSIAMAuthenticatorPath)
			lg.Info("downloading aws-iam-authenticator", zap.String("aws-iam-authenticator-path", cfg.AWSIAMAuthenticatorPath))
			if err = os.RemoveAll(cfg.AWSIAMAuthenticatorPath); err != nil {
				return nil, err
			}
			if err = httputil.Download(lg, os.Stderr, cfg.AWSIAMAuthenticatorDownloadURL, cfg.AWSIAMAuthenticatorPath); err != nil {
				return nil, err
			}
		} else {
			lg.Info("skipping aws-iam-authenticator download; already exist", zap.String("aws-iam-authenticator-path", cfg.AWSIAMAuthenticatorPath))
		}
		if err = fileutil.EnsureExecutable(cfg.AWSIAMAuthenticatorPath); err != nil {
			// file may be already executable while the process does not own the file/directory
			// ref. https://github.com/aws/aws-k8s-tester/issues/66
			lg.Warn("failed to ensure executable", zap.Error(err))
			err = nil
		}
		// aws-iam-authenticator version
		ctx, cancel = context.WithTimeout(context.Background(), 15*time.Second)
		vo, err = exec.New().CommandContext(ctx, cfg.AWSIAMAuthenticatorPath, "version").CombinedOutput()
		cancel()
		if err != nil {
			return nil, fmt.Errorf("'aws-iam-authenticator version' failed (output %q, error %v)", string(vo), err)
		}
		lg.Info(
			"aws-iam-authenticator version",
			zap.String("aws-iam-authenticator-path", cfg.AWSIAMAuthenticatorPath),
			zap.String("aws-iam-authenticator-version", string(vo)),
		)
	}

	ts = &Tester{
		stopCreationCh:     make(chan struct{}),
		stopCreationChOnce: new(sync.Once),
		osSig:              make(chan os.Signal),
		downMu:             new(sync.Mutex),
		lg:                 lg,
		cfg:                cfg,
	}
	signal.Notify(ts.osSig, syscall.SIGTERM, syscall.SIGINT)

	defer ts.cfg.Sync()

	awsCfg := &pkg_aws.Config{
		Logger:        ts.lg,
		DebugAPICalls: ts.cfg.LogLevel == "debug",
		Partition:     ts.cfg.Partition,
		Region:        ts.cfg.Region,
	}
	var stsOutput *sts.GetCallerIdentityOutput
	ts.awsSession, stsOutput, ts.cfg.Status.AWSCredentialPath, err = pkg_aws.New(awsCfg)
	if err != nil {
		return nil, err
	}
	ts.cfg.Status.AWSAccountID = aws.StringValue(stsOutput.Account)
	ts.cfg.Status.AWSUserID = aws.StringValue(stsOutput.UserId)
	ts.cfg.Status.AWSIAMRoleARN = aws.StringValue(stsOutput.Arn)
	ts.cfg.Sync()

	ts.iamAPI = iam.New(ts.awsSession)
	ts.kmsAPI = kms.New(ts.awsSession)
	ts.ssmAPI = ssm.New(ts.awsSession)
	ts.cfnAPI = cloudformation.New(ts.awsSession)

	ts.ec2API = ec2.New(ts.awsSession)
	if _, err = ts.ec2API.DescribeInstances(&ec2.DescribeInstancesInput{MaxResults: aws.Int64(5)}); err != nil {
		return nil, fmt.Errorf("failed to describe instances using EC2 API (%v)", err)
	}
	fmt.Println("EC2 API available!")

	ts.s3API = s3.New(ts.awsSession)
	ts.asgAPI = autoscaling.New(ts.awsSession)
	ts.elbv2API = elbv2.New(ts.awsSession)
	ts.ecrAPI = ecr.New(ts.awsSession)

	ts.lg.Info("checking ECR API availability; listing repositories")
	var ecrResp *ecr.DescribeRepositoriesOutput
	ecrResp, err = ts.ecrAPI.DescribeRepositories(&ecr.DescribeRepositoriesInput{
		MaxResults: aws.Int64(20),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to describe repositories using ECR API (%v)", err)
	}
	ts.lg.Info("listed repositories with limit 20", zap.Int("repositories", len(ecrResp.Repositories)))
	for _, v := range ecrResp.Repositories {
		ts.lg.Info("EKS repository", zap.String("repository-uri", aws.StringValue(v.RepositoryUri)))
	}

	// create a separate session for EKS (for resolver endpoint)
	ts.eksSession, _, ts.cfg.Status.AWSCredentialPath, err = pkg_aws.New(&pkg_aws.Config{
		Logger:        ts.lg,
		DebugAPICalls: ts.cfg.LogLevel == "debug",
		Partition:     ts.cfg.Partition,
		Region:        ts.cfg.Region,
		ResolverURL:   ts.cfg.Parameters.ResolverURL,
		SigningName:   ts.cfg.Parameters.SigningName,
	})
	if err != nil {
		return nil, err
	}
	ts.eksAPI = aws_eks.New(ts.eksSession)

	ts.lg.Info("checking EKS API availability; listing clusters")
	var eksListResp *aws_eks.ListClustersOutput
	eksListResp, err = ts.eksAPI.ListClusters(&aws_eks.ListClustersInput{
		MaxResults: aws.Int64(20),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list clusters using EKS API (%v)", err)
	}
	ts.lg.Info("listed clusters with limit 20", zap.Int("clusters", len(eksListResp.Clusters)))
	for _, v := range eksListResp.Clusters {
		ts.lg.Info("EKS cluster", zap.String("name", aws.StringValue(v)))
	}

	// update k8s client if cluster has already been created
	ts.lg.Info("creating k8s client from previous states if any")
	kcfg := &k8s_client.EKSConfig{
		Logger:            ts.lg,
		Region:            ts.cfg.Region,
		ClusterName:       ts.cfg.Name,
		KubeConfigPath:    ts.cfg.KubeConfigPath,
		KubectlPath:       ts.cfg.KubectlPath,
		ServerVersion:     ts.cfg.Parameters.Version,
		EncryptionEnabled: ts.cfg.Parameters.EncryptionCMKARN != "",
		Clients:           ts.cfg.Clients,
		ClientQPS:         ts.cfg.ClientQPS,
		ClientBurst:       ts.cfg.ClientBurst,
		ClientTimeout:     ts.cfg.ClientTimeout,
	}
	if ts.cfg.Status != nil {
		kcfg.ClusterAPIServerEndpoint = ts.cfg.Status.ClusterAPIServerEndpoint
		kcfg.ClusterCADecoded = ts.cfg.Status.ClusterCADecoded
	}
	ts.k8sClient, err = k8s_client.NewEKS(kcfg)
	if err != nil {
		ts.lg.Warn("failed to create k8s client from previous states", zap.Error(err))
	} else {
		ts.lg.Info("created k8s client from previous states")
		// call here, because "createCluster" won't be called
		// if loaded from previous states
		// e.g. delete
		if err = ts.createSubTesters(); err != nil {
			return nil, err
		}
	}

	return ts, nil
}

func (ts *Tester) createSubTesters() (err error) {
	fmt.Printf("\n*********************************\n")
	fmt.Printf("createSubTesters (%q)\n", ts.cfg.ConfigPath)

	if ts.cfg.IsEnabledAddOnNodeGroups() {
		ts.lg.Info("creating ngTester")
		ts.ngTester, err = ng.New(ng.Config{
			Logger:    ts.lg,
			Stopc:     ts.stopCreationCh,
			EKSConfig: ts.cfg,
			K8SClient: ts.k8sClient,
			IAMAPI:    ts.iamAPI,
			CFNAPI:    ts.cfnAPI,
			EC2API:    ts.ec2API,
			ASGAPI:    ts.asgAPI,
			EKSAPI:    ts.eksAPI,
			SSMAPI:    ts.ssmAPI,
			S3API:     ts.s3API,
		})
		if err != nil {
			return err
		}
	}

	if ts.cfg.IsEnabledAddOnManagedNodeGroups() {
		ts.lg.Info("creating mngTester")
		ts.mngTester, err = mng.New(mng.Config{
			Logger:    ts.lg,
			Stopc:     ts.stopCreationCh,
			EKSConfig: ts.cfg,
			K8SClient: ts.k8sClient,
			IAMAPI:    ts.iamAPI,
			CFNAPI:    ts.cfnAPI,
			EC2API:    ts.ec2API,
			ASGAPI:    ts.asgAPI,
			EKSAPI:    ts.eksAPI,
			S3API:     ts.s3API,
		})
		if err != nil {
			return err
		}
	}

	if ts.cfg.IsEnabledAddOnNodeGroups() || ts.cfg.IsEnabledAddOnManagedNodeGroups() {
		ts.lg.Info("creating gpuTester")
		ts.gpuTester, err = gpu.New(gpu.Config{
			Logger:    ts.lg,
			Stopc:     ts.stopCreationCh,
			EKSConfig: ts.cfg,
			K8SClient: ts.k8sClient,
		})
		if err != nil {
			return err
		}
	}

	if ts.cfg.IsEnabledAddOnConformance() {
		ts.lg.Info("creating conformanceTester")
		ts.conformanceTester, err = conformance.New(conformance.Config{
			Logger:    ts.lg,
			Stopc:     ts.stopCreationCh,
			EKSConfig: ts.cfg,
			K8SClient: ts.k8sClient,
		})
	}

	if ts.cfg.IsEnabledAddOnCSIEBS() {
		ts.lg.Info("creating csiEBSTester")
		ts.csiEBSTester, err = csi_ebs.New(csi_ebs.Config{
			Logger:    ts.lg,
			Stopc:     ts.stopCreationCh,
			EKSConfig: ts.cfg,
			K8SClient: ts.k8sClient,
		})
	}

	if ts.cfg.IsEnabledAddOnAppMesh() {
		ts.lg.Info("creating appMeshTester")
		ts.appMeshTester, err = app_mesh.New(app_mesh.Config{
			Logger:    ts.lg,
			Stopc:     ts.stopCreationCh,
			EKSConfig: ts.cfg,
			K8SClient: ts.k8sClient,
			CFNAPI:    ts.cfnAPI,
		})
	}

	if ts.cfg.IsEnabledAddOnKubernetesDashboard() {
		ts.lg.Info("creating kubernetesDashboardTester")
		ts.kubernetesDashboardTester, err = kubernetes_dashboard.New(kubernetes_dashboard.Config{
			Logger:    ts.lg,
			Stopc:     ts.stopCreationCh,
			EKSConfig: ts.cfg,
			K8SClient: ts.k8sClient,
		})
	}

	if ts.cfg.IsEnabledAddOnPrometheusGrafana() {
		ts.lg.Info("creating prometheusGrafanaTester")
		ts.prometheusGrafanaTester, err = prometheus_grafana.New(prometheus_grafana.Config{
			Logger:    ts.lg,
			Stopc:     ts.stopCreationCh,
			EKSConfig: ts.cfg,
			K8SClient: ts.k8sClient,
		})
	}

	if ts.cfg.IsEnabledAddOnNLBHelloWorld() {
		ts.lg.Info("creating nlbHelloWorldTester")
		ts.nlbHelloWorldTester, err = nlb_hello_world.New(nlb_hello_world.Config{
			Logger:    ts.lg,
			Stopc:     ts.stopCreationCh,
			EKSConfig: ts.cfg,
			K8SClient: ts.k8sClient,
			ELB2API:   ts.elbv2API,
		})
		if err != nil {
			return err
		}
	}

	if ts.cfg.IsEnabledAddOnALB2048() {
		ts.lg.Info("creating alb2048Tester")
		ts.alb2048Tester, err = alb_2048.New(alb_2048.Config{
			Logger:    ts.lg,
			Stopc:     ts.stopCreationCh,
			CFNAPI:    ts.cfnAPI,
			EKSConfig: ts.cfg,
			K8SClient: ts.k8sClient,
			ELB2API:   ts.elbv2API,
		})
		if err != nil {
			return err
		}
	}

	if ts.cfg.IsEnabledAddOnJobsPi() {
		ts.lg.Info("creating jobsPiTester")
		ts.jobsPiTester, err = jobs_pi.New(jobs_pi.Config{
			Logger:    ts.lg,
			Stopc:     ts.stopCreationCh,
			EKSConfig: ts.cfg,
			K8SClient: ts.k8sClient,
		})
		if err != nil {
			return err
		}
	}

	if ts.cfg.IsEnabledAddOnJobsEcho() {
		ts.lg.Info("creating jobsEchoTester")
		ts.jobsEchoTester, err = jobs_echo.New(jobs_echo.Config{
			Logger:    ts.lg,
			Stopc:     ts.stopCreationCh,
			EKSConfig: ts.cfg,
			K8SClient: ts.k8sClient,
		})
		if err != nil {
			return err
		}
	}

	if ts.cfg.IsEnabledAddOnCronJobs() {
		ts.lg.Info("creating cronJobsTester")
		ts.cronJobsTester, err = cron_jobs.New(cron_jobs.Config{
			Logger:    ts.lg,
			Stopc:     ts.stopCreationCh,
			EKSConfig: ts.cfg,
			K8SClient: ts.k8sClient,
		})
		if err != nil {
			return err
		}
	}

	if ts.cfg.IsEnabledAddOnCSRsLocal() {
		ts.lg.Info("creating csrsLocalTester")
		ts.csrsLocalTester, err = csrs_local.New(csrs_local.Config{
			Logger:    ts.lg,
			Stopc:     ts.stopCreationCh,
			EKSConfig: ts.cfg,
			K8SClient: ts.k8sClient,
		})
		if err != nil {
			return err
		}
	}
	if ts.cfg.IsEnabledAddOnCSRsRemote() {
		ts.lg.Info("creating csrsRemoteTester")
		ts.csrsRemoteTester, err = csrs_remote.New(csrs_remote.Config{
			Logger:    ts.lg,
			Stopc:     ts.stopCreationCh,
			EKSConfig: ts.cfg,
			K8SClient: ts.k8sClient,
			ECRAPI:    ts.ecrAPI,
		})
		if err != nil {
			return err
		}
	}

	if ts.cfg.IsEnabledAddOnConfigMapsLocal() {
		ts.lg.Info("creating configMapsLocalTester")
		ts.configMapsLocalTester, err = config_maps_local.New(config_maps_local.Config{
			Logger:    ts.lg,
			Stopc:     ts.stopCreationCh,
			EKSConfig: ts.cfg,
			K8SClient: ts.k8sClient,
		})
		if err != nil {
			return err
		}
	}
	if ts.cfg.IsEnabledAddOnConfigMapsRemote() {
		ts.lg.Info("creating configMapsRemoteTester")
		ts.configMapsRemoteTester, err = config_maps_remote.New(config_maps_remote.Config{
			Logger:    ts.lg,
			Stopc:     ts.stopCreationCh,
			EKSConfig: ts.cfg,
			K8SClient: ts.k8sClient,
			ECRAPI:    ts.ecrAPI,
		})
		if err != nil {
			return err
		}
	}

	if ts.cfg.IsEnabledAddOnSecretsLocal() {
		ts.lg.Info("creating secretsLocalTester")
		ts.secretsLocalTester, err = secrets_local.New(secrets_local.Config{
			Logger:    ts.lg,
			Stopc:     ts.stopCreationCh,
			EKSConfig: ts.cfg,
			K8SClient: ts.k8sClient,
		})
		if err != nil {
			return err
		}
	}
	if ts.cfg.IsEnabledAddOnSecretsRemote() {
		ts.lg.Info("creating secretsRemoteTester")
		ts.secretsRemoteTester, err = secrets_remote.New(secrets_remote.Config{
			Logger:    ts.lg,
			Stopc:     ts.stopCreationCh,
			EKSConfig: ts.cfg,
			K8SClient: ts.k8sClient,
			ECRAPI:    ts.ecrAPI,
		})
		if err != nil {
			return err
		}
	}

	if ts.cfg.IsEnabledAddOnFargate() {
		ts.lg.Info("creating fargateTester")
		ts.fargateTester, err = fargate.New(fargate.Config{
			Logger:    ts.lg,
			Stopc:     ts.stopCreationCh,
			EKSConfig: ts.cfg,
			K8SClient: ts.k8sClient,
			IAMAPI:    ts.iamAPI,
			CFNAPI:    ts.cfnAPI,
			EKSAPI:    ts.eksAPI,
			ECRAPI:    ts.ecrAPI,
		})
		if err != nil {
			return err
		}
	}

	if ts.cfg.IsEnabledAddOnIRSA() {
		ts.lg.Info("creating irsaTester")
		ts.irsaTester, err = irsa.New(irsa.Config{
			Logger:    ts.lg,
			Stopc:     ts.stopCreationCh,
			EKSConfig: ts.cfg,
			K8SClient: ts.k8sClient,
			CFNAPI:    ts.cfnAPI,
			IAMAPI:    ts.iamAPI,
			S3API:     ts.s3API,
			ECRAPI:    ts.ecrAPI,
		})
		if err != nil {
			return err
		}
	}

	if ts.cfg.IsEnabledAddOnIRSAFargate() {
		ts.lg.Info("creating irsaFargateTester")
		ts.irsaFargateTester, err = irsa_fargate.New(irsa_fargate.Config{
			Logger:    ts.lg,
			Stopc:     ts.stopCreationCh,
			EKSConfig: ts.cfg,
			K8SClient: ts.k8sClient,
			IAMAPI:    ts.iamAPI,
			CFNAPI:    ts.cfnAPI,
			EKSAPI:    ts.eksAPI,
			S3API:     ts.s3API,
			ECRAPI:    ts.ecrAPI,
		})
		if err != nil {
			return err
		}
	}

	if ts.cfg.IsEnabledAddOnWordpress() {
		ts.lg.Info("creating wordPressTester")
		ts.wordPressTester, err = wordpress.New(wordpress.Config{
			Logger:    ts.lg,
			Stopc:     ts.stopCreationCh,
			EKSConfig: ts.cfg,
			K8SClient: ts.k8sClient,
		})
		if err != nil {
			return err
		}
	}

	if ts.cfg.IsEnabledAddOnJupyterHub() {
		ts.lg.Info("creating jupyterHubTester")
		ts.jupyterHubTester, err = jupyter_hub.New(jupyter_hub.Config{
			Logger:    ts.lg,
			Stopc:     ts.stopCreationCh,
			EKSConfig: ts.cfg,
			K8SClient: ts.k8sClient,
		})
	}

	if ts.cfg.IsEnabledAddOnKubeflow() {
		ts.lg.Info("creating kubeflowTester")
		ts.kubeflowTester, err = kubeflow.New(kubeflow.Config{
			Logger:    ts.lg,
			Stopc:     ts.stopCreationCh,
			EKSConfig: ts.cfg,
			K8SClient: ts.k8sClient,
		})
		if err != nil {
			return err
		}
	}

	if ts.cfg.IsEnabledAddOnClusterLoaderLocal() {
		ts.lg.Info("creating clusterLoaderLocalTester")
		ts.clusterLoaderLocalTester = cluster_loader_local.New(cluster_loader_local.Config{
			Logger:    ts.lg,
			Stopc:     ts.stopCreationCh,
			EKSConfig: ts.cfg,
			K8SClient: ts.k8sClient,
		})
	}
	if ts.cfg.IsEnabledAddOnClusterLoaderRemote() {
		ts.lg.Info("creating clusterLoaderRemoteTester")
		ts.clusterLoaderRemoteTester, err = cluster_loader_remote.New(cluster_loader_remote.Config{
			Logger:    ts.lg,
			Stopc:     ts.stopCreationCh,
			EKSConfig: ts.cfg,
			K8SClient: ts.k8sClient,
			ECRAPI:    ts.ecrAPI,
		})
		if err != nil {
			return err
		}
	}

	if ts.cfg.IsEnabledAddOnHollowNodesLocal() {
		ts.lg.Info("creating hollowNodesLocalTester")
		ts.hollowNodesLocalTester, err = hollow_nodes_local.New(hollow_nodes_local.Config{
			Logger:    ts.lg,
			Stopc:     ts.stopCreationCh,
			EKSConfig: ts.cfg,
			K8SClient: ts.k8sClient,
		})
		if err != nil {
			return err
		}
	}
	if ts.cfg.IsEnabledAddOnHollowNodesRemote() {
		ts.lg.Info("creating hollowNodesRemoteTester")
		ts.hollowNodesRemoteTester, err = hollow_nodes_remote.New(hollow_nodes_remote.Config{
			Logger:    ts.lg,
			Stopc:     ts.stopCreationCh,
			EKSConfig: ts.cfg,
			K8SClient: ts.k8sClient,
			ECRAPI:    ts.ecrAPI,
		})
		if err != nil {
			return err
		}
	}

	if ts.cfg.IsEnabledAddOnStresserLocal() {
		ts.lg.Info("creating stresserLocalTester")
		ts.stresserLocalTester, err = stresser_local.New(stresser_local.Config{
			Logger:    ts.lg,
			Stopc:     ts.stopCreationCh,
			EKSConfig: ts.cfg,
			K8SClient: ts.k8sClient,
		})
		if err != nil {
			return err
		}
	}
	if ts.cfg.IsEnabledAddOnStresserRemote() {
		ts.lg.Info("creating stresserRemoteTester")
		ts.stresserRemoteTester, err = stresser_remote.New(stresser_remote.Config{
			Logger:    ts.lg,
			Stopc:     ts.stopCreationCh,
			EKSConfig: ts.cfg,
			K8SClient: ts.k8sClient,
			ECRAPI:    ts.ecrAPI,
		})
		if err != nil {
			return err
		}
	}

	return ts.cfg.Sync()
}

// Up should provision a new cluster for testing.
// ref. https://pkg.go.dev/k8s.io/test-infra/kubetest2/pkg/types?tab=doc#Deployer
// ref. https://pkg.go.dev/k8s.io/test-infra/kubetest2/pkg/types?tab=doc#Options
func (ts *Tester) Up() (err error) {
	fmt.Printf("\n*********************************\n")
	fmt.Printf("UP START (%q)\n", ts.cfg.ConfigPath)

	now := time.Now()

	defer func() {
		fmt.Printf("\n*********************************\n")
		fmt.Printf("UP DEFER START (%q, %q)\n\n", ts.cfg.ConfigPath, ts.cfg.KubectlCommand())

		if serr := ts.uploadToS3(); serr != nil {
			ts.lg.Warn("failed to upload artifacts to S3", zap.Error(serr))
		}

		if err == nil {
			if ts.cfg.Status.Up {
				fmt.Printf("\n*********************************\n")
				ts.lg.Sugar().Infof("SSH (%s)", ts.cfg.ConfigPath)
				fmt.Println(ts.cfg.SSHCommands())

				fmt.Printf("\n*********************************\n")
				ts.lg.Sugar().Infof("kubectl (%s)", ts.cfg.ConfigPath)
				fmt.Println(ts.cfg.KubectlCommands())

				ts.lg.Info("Up succeeded",
					zap.String("started", humanize.RelTime(now, time.Now(), "ago", "from now")),
				)

				fmt.Printf("\n*********************************\n")
				ts.lg.Sugar().Infof("Up.defer end (%s, %s)", ts.cfg.ConfigPath, ts.cfg.KubectlCommand())
				fmt.Printf("\n\n💯 😁 👍 :)  UP SUCCESS\n\n\n")
			} else {
				fmt.Printf("\n\n😲 😲 😲  UP ABORTED ???\n\n\n")
			}
			fmt.Printf("\n\n# to delete cluster\naws-k8s-tester eks delete cluster --path %s\n\n", ts.cfg.ConfigPath)
			return
		}

		if !ts.cfg.OnFailureDelete {
			if ts.cfg.Status.Up {
				fmt.Printf("\n*********************************\n")
				ts.lg.Sugar().Infof("SSH (%s)", ts.cfg.ConfigPath)
				fmt.Println(ts.cfg.SSHCommands())

				fmt.Printf("\n*********************************\n")
				ts.lg.Sugar().Infof("kubectl (%s)", ts.cfg.ConfigPath)
				fmt.Println(ts.cfg.KubectlCommands())
			}

			ts.lg.Warn("Up failed",
				zap.String("started", humanize.RelTime(now, time.Now(), "ago", "from now")),
				zap.Error(err),
			)

			fmt.Printf("\n\n\nUP FAIL ERROR:\n\n%v\n\n\n", err)

			fmt.Printf("\n*********************************\n")
			ts.lg.Sugar().Infof("Up.defer end (%s, %s)", ts.cfg.ConfigPath, ts.cfg.KubectlCommand())
			fmt.Printf("\n\n🔥 💀 👽 😱 😡 (-_-)  UP FAIL\n\n\n")
			fmt.Printf("\n\n# to delete cluster\naws-k8s-tester eks delete cluster --path %s\n\n", ts.cfg.ConfigPath)
			return
		}

		if ts.cfg.Status.Up {
			fmt.Printf("\n*********************************\n")
			ts.lg.Sugar().Infof("SSH (%s)", ts.cfg.ConfigPath)
			fmt.Println(ts.cfg.SSHCommands())

			fmt.Printf("\n*********************************\n")
			ts.lg.Sugar().Infof("kubectl (%s)", ts.cfg.ConfigPath)
			fmt.Println(ts.cfg.KubectlCommands())
		}

		fmt.Printf("\n\n\nUP FAIL ERROR:\n\n%v\n\n\n", err)

		fmt.Printf("\n*********************************\n")
		fmt.Printf("🔥 💀 👽 😱 😡 (-_-) UP FAIL\n")
		ts.lg.Warn("Up failed; reverting resource creation",
			zap.String("started", humanize.RelTime(now, time.Now(), "ago", "from now")),
			zap.Error(err),
		)
		waitDur := time.Duration(ts.cfg.OnFailureDeleteWaitSeconds) * time.Second
		if waitDur > 0 {
			ts.lg.Info("waiting before clean up", zap.Duration("wait", waitDur))
			select {
			case <-ts.stopCreationCh:
				ts.lg.Info("wait aborted before clean up")
			case <-ts.osSig:
				ts.lg.Info("wait aborted before clean up")
			case <-time.After(waitDur):
			}
		}
		derr := ts.down()
		if derr != nil {
			ts.lg.Warn("failed to revert Up", zap.Error(derr))
		} else {
			ts.lg.Warn("reverted Up")
		}

		fmt.Printf("\n\n\nUP FAIL ERROR:\n\n%v\n\n\n", err)

		fmt.Printf("\n*********************************\n")
		ts.lg.Sugar().Infof("Up.defer end (%s, %s)", ts.cfg.ConfigPath, ts.cfg.KubectlCommand())
		fmt.Printf("\n\n🔥 💀 👽 😱 😡 (-_-) Up fail\n\n\n")
	}()

	ts.lg.Info("Up started",
		zap.String("version", version.Version()),
		zap.String("name", ts.cfg.Name),
	)
	defer ts.cfg.Sync()

	fmt.Printf("\n*********************************\n")
	fmt.Printf("createS3 (%q)\n", ts.cfg.ConfigPath)
	if err := catchInterrupt(
		ts.lg,
		ts.stopCreationCh,
		ts.stopCreationChOnce,
		ts.osSig,
		ts.createS3,
	); err != nil {
		return err
	}

	fmt.Printf("\n*********************************\n")
	fmt.Printf("createEncryption (%q)\n", ts.cfg.ConfigPath)
	if err := catchInterrupt(
		ts.lg,
		ts.stopCreationCh,
		ts.stopCreationChOnce,
		ts.osSig,
		ts.createEncryption,
	); err != nil {
		return err
	}

	fmt.Printf("\n*********************************\n")
	fmt.Printf("createKeyPair (%q)\n", ts.cfg.ConfigPath)
	if err := catchInterrupt(
		ts.lg,
		ts.stopCreationCh,
		ts.stopCreationChOnce,
		ts.osSig,
		ts.createKeyPair,
	); err != nil {
		return err
	}

	fmt.Printf("\n*********************************\n")
	fmt.Printf("createClusterRole (%q)\n", ts.cfg.ConfigPath)
	if err := catchInterrupt(
		ts.lg,
		ts.stopCreationCh,
		ts.stopCreationChOnce,
		ts.osSig,
		ts.createClusterRole,
	); err != nil {
		return err
	}

	fmt.Printf("\n*********************************\n")
	fmt.Printf("createVPC (%q)\n", ts.cfg.ConfigPath)
	if err := catchInterrupt(
		ts.lg,
		ts.stopCreationCh,
		ts.stopCreationChOnce,
		ts.osSig,
		ts.createVPC,
	); err != nil {
		return err
	}

	fmt.Printf("\n*********************************\n")
	fmt.Printf("createCluster (%q, %q)\n", ts.cfg.ConfigPath, ts.cfg.KubectlCommand())
	if err := catchInterrupt(
		ts.lg,
		ts.stopCreationCh,
		ts.stopCreationChOnce,
		ts.osSig,
		ts.createCluster,
	); err != nil {
		return err
	}

	waitDur := 30 * time.Second
	ts.lg.Info("waiting before running health check", zap.Duration("wait", waitDur))
	time.Sleep(waitDur)

	fmt.Printf("\n*********************************\n")
	fmt.Printf("checkHealth (%q, %q)\n", ts.cfg.ConfigPath, ts.cfg.KubectlCommand())
	if err := catchInterrupt(
		ts.lg,
		ts.stopCreationCh,
		ts.stopCreationChOnce,
		ts.osSig,
		ts.checkHealth,
	); err != nil {
		return err
	}

	if ts.cfg.CommandAfterCreateCluster != "" {
		if err := ts.cfg.EvaluateCommandRefs(); err != nil {
			return err
		}
		fmt.Printf("\n*********************************\n")
		fmt.Printf("\nrunCommand CommandAfterCreateCluster (%q)\n", ts.cfg.CommandAfterCreateCluster)
		out, err := runCommand(ts.lg, ts.cfg.CommandAfterCreateCluster, ts.cfg.CommandAfterCreateClusterTimeout)
		if err != nil {
			err = ioutil.WriteFile(ts.cfg.CommandAfterCreateClusterOutputPath, []byte(ts.cfg.CommandAfterCreateCluster+"\n\n# output\n"+string(out)+"\n\n# error\n"+err.Error()), 0600)
			if err != nil {
				return fmt.Errorf("failed to write file %q (%v)", ts.cfg.CommandAfterCreateClusterOutputPath, err)
			}
		} else {
			err = ioutil.WriteFile(ts.cfg.CommandAfterCreateClusterOutputPath, []byte(ts.cfg.CommandAfterCreateCluster+"\n\n# output\n"+string(out)), 0600)
			if err != nil {
				return fmt.Errorf("failed to write file %q (%v)", ts.cfg.CommandAfterCreateClusterOutputPath, err)
			}
		}
		fmt.Printf("\nrunCommand output:\n\n%s\n", string(out))
	}

	if ts.cfg.IsEnabledAddOnNodeGroups() {
		if ts.ngTester == nil {
			return errors.New("ts.ngTester == nil when AddOnNodeGroups.Enable == true")
		}
		// create NG first, so MNG configmap update can be called afterwards
		fmt.Printf("\n*********************************\n")
		fmt.Printf("ngTester.Create (%q, %q)\n", ts.cfg.ConfigPath, ts.cfg.KubectlCommand())
		if err := catchInterrupt(
			ts.lg,
			ts.stopCreationCh,
			ts.stopCreationChOnce,
			ts.osSig,
			ts.ngTester.Create,
		); err != nil {
			return err
		}
	}

	if ts.cfg.IsEnabledAddOnManagedNodeGroups() {
		if ts.mngTester == nil {
			return errors.New("ts.mngTester == nil when AddOnManagedNodeGroups.Enable == true")
		}
		fmt.Printf("\n*********************************\n")
		fmt.Printf("mngTester.Create (%q, %q)\n", ts.cfg.ConfigPath, ts.cfg.KubectlCommand())
		if err := catchInterrupt(
			ts.lg,
			ts.stopCreationCh,
			ts.stopCreationChOnce,
			ts.osSig,
			ts.mngTester.Create,
		); err != nil {
			return err
		}
	}

	needGPU := false
	if ts.cfg.IsEnabledAddOnNodeGroups() {
	gpuFound1:
		for _, mv := range ts.cfg.AddOnNodeGroups.ASGs {
			switch mv.AMIType {
			case ec2config.AMITypeAL2X8664GPU:
				needGPU = true
				break gpuFound1
			}
		}
	}
	if !needGPU && ts.cfg.IsEnabledAddOnManagedNodeGroups() {
	gpuFound2:
		for _, mv := range ts.cfg.AddOnManagedNodeGroups.MNGs {
			switch mv.AMIType {
			case aws_eks.AMITypesAl2X8664Gpu:
				needGPU = true
				break gpuFound2
			}
		}
	}
	if needGPU {
		fmt.Printf("\n*********************************\n")
		fmt.Printf("gpuTester.InstallNvidiaDriver (%q, %q)\n", ts.cfg.ConfigPath, ts.cfg.KubectlCommand())
		if err := catchInterrupt(
			ts.lg,
			ts.stopCreationCh,
			ts.stopCreationChOnce,
			ts.osSig,
			ts.gpuTester.InstallNvidiaDriver,
		); err != nil {
			ts.lg.Warn("failed to install nvidia driver", zap.Error(err))
			return err
		}

		fmt.Printf("\n*********************************\n")
		fmt.Printf("gpuTester.CreateNvidiaSMI (%q, %q)\n", ts.cfg.ConfigPath, ts.cfg.KubectlCommand())
		if err := catchInterrupt(
			ts.lg,
			ts.stopCreationCh,
			ts.stopCreationChOnce,
			ts.osSig,
			ts.gpuTester.CreateNvidiaSMI,
		); err != nil {
			ts.lg.Warn("failed to create nvidia-smi", zap.Error(err))
			return err
		}
	}

	if ts.cfg.IsEnabledAddOnConformance() {
		if ts.conformanceTester == nil {
			return errors.New("ts.conformanceTester == nil when AddOnConformance.Enable == true")
		}
		fmt.Printf("\n*********************************\n")
		fmt.Printf("conformanceTester.Create (%q, \"%s --namespace=%s get all\")\n", ts.cfg.ConfigPath, ts.cfg.KubectlCommand(), ts.cfg.AddOnConformance.Namespace)
		err := catchInterrupt(
			ts.lg,
			ts.stopCreationCh,
			ts.stopCreationChOnce,
			ts.osSig,
			ts.conformanceTester.Create,
		)

		fmt.Printf("\n*********************************\n")
		ts.lg.Sugar().Infof("SSH (%s)", ts.cfg.ConfigPath)
		fmt.Println(ts.cfg.SSHCommands())

		fmt.Printf("\n*********************************\n")
		ts.lg.Sugar().Infof("kubectl (%s)", ts.cfg.ConfigPath)
		fmt.Println(ts.cfg.KubectlCommands())

		fmt.Printf("\n*********************************\n")
		if serr := ts.uploadToS3(); serr != nil {
			ts.lg.Warn("failed to upload artifacts to S3", zap.Error(serr))
		}

		if err != nil {
			return err
		}
	}

	if ts.cfg.IsEnabledAddOnCSIEBS() {
		if ts.csiEBSTester == nil {
			return errors.New("ts.csiEBSTester == nil when AddOnCSIEBS.Enable == true")
		}
		fmt.Printf("\n*********************************\n")
		fmt.Printf("csiEBSTester.Create (%q, \"%s --namespace=kube-system get all\")\n", ts.cfg.ConfigPath, ts.cfg.KubectlCommand())
		if err := catchInterrupt(
			ts.lg,
			ts.stopCreationCh,
			ts.stopCreationChOnce,
			ts.osSig,
			ts.csiEBSTester.Create,
		); err != nil {
			return err
		}
	}

	if ts.cfg.IsEnabledAddOnAppMesh() {
		if ts.appMeshTester == nil {
			return errors.New("ts.appMeshTester == nil when AddOnAppMesh.Enable == true")
		}
		fmt.Printf("\n*********************************\n")
		fmt.Printf("appMeshTester.Create (%q, \"%s --namespace=%s get all\")\n", ts.cfg.ConfigPath, ts.cfg.KubectlCommand(), ts.cfg.AddOnAppMesh.Namespace)
		if err := catchInterrupt(
			ts.lg,
			ts.stopCreationCh,
			ts.stopCreationChOnce,
			ts.osSig,
			ts.appMeshTester.Create,
		); err != nil {
			return err
		}
	}

	if ts.cfg.IsEnabledAddOnKubernetesDashboard() {
		if ts.kubernetesDashboardTester == nil {
			return errors.New("ts.kubernetesDashboardTester == nil when AddOnKubernetesDashboard.Enable == true")
		}
		fmt.Printf("\n*********************************\n")
		fmt.Printf("kubernetesDashboardTester.Create (%q, \"%s --namespace=kube-system get all\")\n", ts.cfg.ConfigPath, ts.cfg.KubectlCommand())
		if err := catchInterrupt(
			ts.lg,
			ts.stopCreationCh,
			ts.stopCreationChOnce,
			ts.osSig,
			ts.kubernetesDashboardTester.Create,
		); err != nil {
			return err
		}
	}

	if ts.cfg.IsEnabledAddOnPrometheusGrafana() {
		if ts.prometheusGrafanaTester == nil {
			return errors.New("ts.prometheusGrafanaTester == nil when AddOnKubernetesDashboard.Enable == true")
		}
		fmt.Printf("\n*********************************\n")
		fmt.Printf("prometheusGrafanaTester.Create (%q, \"%s --namespace=prometheus/grafana get all\")\n", ts.cfg.ConfigPath, ts.cfg.KubectlCommand())
		if err := catchInterrupt(
			ts.lg,
			ts.stopCreationCh,
			ts.stopCreationChOnce,
			ts.osSig,
			ts.prometheusGrafanaTester.Create,
		); err != nil {
			return err
		}
	}

	fmt.Printf("\n*********************************\n")
	fmt.Printf("checkHealth (%q, %q)\n", ts.cfg.ConfigPath, ts.cfg.KubectlCommand())
	if err := catchInterrupt(
		ts.lg,
		ts.stopCreationCh,
		ts.stopCreationChOnce,
		ts.osSig,
		ts.checkHealth,
	); err != nil {
		return err
	}

	fmt.Printf("\n*********************************\n")
	ts.lg.Sugar().Infof("SSH (%s)", ts.cfg.ConfigPath)
	fmt.Println(ts.cfg.SSHCommands())

	fmt.Printf("\n*********************************\n")
	ts.lg.Sugar().Infof("kubectl (%s)", ts.cfg.ConfigPath)
	fmt.Println(ts.cfg.KubectlCommands())

	fmt.Printf("\n*********************************\n")
	if serr := ts.uploadToS3(); serr != nil {
		ts.lg.Warn("failed to upload artifacts to S3", zap.Error(serr))
	}

	if ts.cfg.IsEnabledAddOnNLBHelloWorld() {
		if ts.nlbHelloWorldTester == nil {
			return errors.New("ts.nlbHelloWorldTester == nil when AddOnNLBHelloWorld.Enable == true")
		}
		fmt.Printf("\n*********************************\n")
		fmt.Printf("nlbHelloWorldTester.Create (%q, \"%s --namespace=%s get all\")\n", ts.cfg.ConfigPath, ts.cfg.KubectlCommand(), ts.cfg.AddOnNLBHelloWorld.Namespace)
		if err := catchInterrupt(
			ts.lg,
			ts.stopCreationCh,
			ts.stopCreationChOnce,
			ts.osSig,
			ts.nlbHelloWorldTester.Create,
		); err != nil {
			return err
		}
	}

	if ts.cfg.IsEnabledAddOnALB2048() {
		if ts.alb2048Tester == nil {
			return errors.New("ts.alb2048Tester == nil when AddOnALB2048.Enable == true")
		}
		fmt.Printf("\n*********************************\n")
		fmt.Printf("alb2048Tester.Create (%q, \"%s --namespace=%s get all\")\n", ts.cfg.ConfigPath, ts.cfg.KubectlCommand(), ts.cfg.AddOnALB2048.Namespace)
		if err := catchInterrupt(
			ts.lg,
			ts.stopCreationCh,
			ts.stopCreationChOnce,
			ts.osSig,
			ts.alb2048Tester.Create,
		); err != nil {
			return err
		}
	}

	if ts.cfg.IsEnabledAddOnJobsPi() {
		if ts.jobsPiTester == nil {
			return errors.New("ts.jobsPiTester == nil when AddOnJobsPi.Enable == true")
		}
		fmt.Printf("\n*********************************\n")
		fmt.Printf("jobsPiTester.Create (%q, \"%s --namespace=%s get all\")\n", ts.cfg.ConfigPath, ts.cfg.KubectlCommand(), ts.cfg.AddOnJobsPi.Namespace)
		if err := catchInterrupt(
			ts.lg,
			ts.stopCreationCh,
			ts.stopCreationChOnce,
			ts.osSig,
			ts.jobsPiTester.Create,
		); err != nil {
			return err
		}
	}

	if ts.cfg.IsEnabledAddOnJobsEcho() {
		if ts.jobsEchoTester == nil {
			return errors.New("ts.jobsEchoTester == nil when AddOnJobsEcho.Enable == true")
		}
		fmt.Printf("\n*********************************\n")
		fmt.Printf("jobsEchoTester.Create (%q, \"%s --namespace=%s get all\")\n", ts.cfg.ConfigPath, ts.cfg.KubectlCommand(), ts.cfg.AddOnJobsEcho.Namespace)
		if err := catchInterrupt(
			ts.lg,
			ts.stopCreationCh,
			ts.stopCreationChOnce,
			ts.osSig,
			ts.jobsEchoTester.Create,
		); err != nil {
			return err
		}
	}

	if ts.cfg.IsEnabledAddOnCronJobs() {
		if ts.cronJobsTester == nil {
			return errors.New("ts.cronJobsTester == nil when AddOnCronJobs.Enable == true")
		}
		fmt.Printf("\n*********************************\n")
		fmt.Printf("cronJobsTester.Create (%q, \"%s --namespace=%s get all\")\n", ts.cfg.ConfigPath, ts.cfg.KubectlCommand(), ts.cfg.AddOnCronJobs.Namespace)
		if err := catchInterrupt(
			ts.lg,
			ts.stopCreationCh,
			ts.stopCreationChOnce,
			ts.osSig,
			ts.cronJobsTester.Create,
		); err != nil {
			return err
		}
	}

	if ts.cfg.IsEnabledAddOnCSRsLocal() {
		if ts.csrsLocalTester == nil {
			return errors.New("ts.csrsLocalTester == nil when AddOnCSRsLocal.Enable == true")
		}
		fmt.Printf("\n*********************************\n")
		fmt.Printf("csrsLocalTester.Create (%q, \"%s get all\")\n", ts.cfg.ConfigPath, ts.cfg.KubectlCommand())
		if err := catchInterrupt(
			ts.lg,
			ts.stopCreationCh,
			ts.stopCreationChOnce,
			ts.osSig,
			ts.csrsLocalTester.Create,
		); err != nil {
			return err
		}
	}
	if ts.cfg.IsEnabledAddOnCSRsRemote() {
		if ts.csrsRemoteTester == nil {
			return errors.New("ts.csrsRemoteTester == nil when AddOnCSRsRemote.Enable == true")
		}
		fmt.Printf("\n*********************************\n")
		fmt.Printf("csrsRemoteTester.Create (%q, \"%s --namespace=%s get all\")\n", ts.cfg.ConfigPath, ts.cfg.KubectlCommand(), ts.cfg.AddOnCSRsRemote.Namespace)
		if err := catchInterrupt(
			ts.lg,
			ts.stopCreationCh,
			ts.stopCreationChOnce,
			ts.osSig,
			ts.csrsRemoteTester.Create,
		); err != nil {
			return err
		}
	}

	if ts.cfg.IsEnabledAddOnConfigMapsLocal() {
		if ts.configMapsLocalTester == nil {
			return errors.New("ts.configMapsLocalTester == nil when AddOnConfigMapsLocal.Enable == true")
		}
		fmt.Printf("\n*********************************\n")
		fmt.Printf("configMapsLocalTester.Create (%q, \"%s --namespace=%s get all\")\n", ts.cfg.ConfigPath, ts.cfg.KubectlCommand(), ts.cfg.AddOnConfigMapsLocal.Namespace)
		if err := catchInterrupt(
			ts.lg,
			ts.stopCreationCh,
			ts.stopCreationChOnce,
			ts.osSig,
			ts.configMapsLocalTester.Create,
		); err != nil {
			return err
		}
	}
	if ts.cfg.IsEnabledAddOnConfigMapsRemote() {
		if ts.configMapsRemoteTester == nil {
			return errors.New("ts.configMapsRemoteTester == nil when AddOnConfigMapsRemote.Enable == true")
		}
		fmt.Printf("\n*********************************\n")
		fmt.Printf("configMapsRemoteTester.Create (%q, \"%s --namespace=%s get all\")\n", ts.cfg.ConfigPath, ts.cfg.KubectlCommand(), ts.cfg.AddOnConfigMapsRemote.Namespace)
		if err := catchInterrupt(
			ts.lg,
			ts.stopCreationCh,
			ts.stopCreationChOnce,
			ts.osSig,
			ts.configMapsRemoteTester.Create,
		); err != nil {
			return err
		}
	}

	if ts.cfg.IsEnabledAddOnSecretsLocal() {
		if ts.secretsLocalTester == nil {
			return errors.New("ts.secretsLocalTester == nil when AddOnSecretsLocal.Enable == true")
		}
		fmt.Printf("\n*********************************\n")
		fmt.Printf("secretsLocalTester.Create (%q, \"%s --namespace=%s get all\")\n", ts.cfg.ConfigPath, ts.cfg.KubectlCommand(), ts.cfg.AddOnSecretsLocal.Namespace)
		if err := catchInterrupt(
			ts.lg,
			ts.stopCreationCh,
			ts.stopCreationChOnce,
			ts.osSig,
			ts.secretsLocalTester.Create,
		); err != nil {
			return err
		}
	}
	if ts.cfg.IsEnabledAddOnSecretsRemote() {
		if ts.secretsRemoteTester == nil {
			return errors.New("ts.secretsRemoteTester == nil when AddOnSecretsRemote.Enable == true")
		}
		fmt.Printf("\n*********************************\n")
		fmt.Printf("secretsRemoteTester.Create (%q, \"%s --namespace=%s get all\")\n", ts.cfg.ConfigPath, ts.cfg.KubectlCommand(), ts.cfg.AddOnSecretsRemote.Namespace)
		if err := catchInterrupt(
			ts.lg,
			ts.stopCreationCh,
			ts.stopCreationChOnce,
			ts.osSig,
			ts.secretsRemoteTester.Create,
		); err != nil {
			return err
		}
	}

	if ts.cfg.IsEnabledAddOnFargate() {
		if ts.fargateTester == nil {
			return errors.New("ts.fargateTester == nil when AddOnFargate.Enable == true")
		}
		fmt.Printf("\n*********************************\n")
		fmt.Printf("fargateTester.Create (%q, \"%s --namespace=%s get all\")\n", ts.cfg.ConfigPath, ts.cfg.KubectlCommand(), ts.cfg.AddOnFargate.Namespace)
		if err := catchInterrupt(
			ts.lg,
			ts.stopCreationCh,
			ts.stopCreationChOnce,
			ts.osSig,
			ts.fargateTester.Create,
		); err != nil {
			return err
		}
	}

	if ts.cfg.IsEnabledAddOnIRSA() {
		if ts.irsaTester == nil {
			return errors.New("ts.irsaTester == nil when AddOnIRSA.Enable == true")
		}
		fmt.Printf("\n*********************************\n")
		fmt.Printf("irsaTester.Create (%q, \"%s --namespace=%s get all\")\n", ts.cfg.ConfigPath, ts.cfg.KubectlCommand(), ts.cfg.AddOnIRSA.Namespace)
		if err := catchInterrupt(
			ts.lg,
			ts.stopCreationCh,
			ts.stopCreationChOnce,
			ts.osSig,
			ts.irsaTester.Create,
		); err != nil {
			return err
		}
	}

	if ts.cfg.IsEnabledAddOnIRSAFargate() {
		if ts.irsaFargateTester == nil {
			return errors.New("ts.irsaFargateTester == nil when AddOnIRSAFargate.Enable == true")
		}
		fmt.Printf("\n*********************************\n")
		fmt.Printf("irsaFargateTester.Create (%q, \"%s --namespace=%s get all\")\n", ts.cfg.ConfigPath, ts.cfg.KubectlCommand(), ts.cfg.AddOnIRSAFargate.Namespace)
		if err := catchInterrupt(
			ts.lg,
			ts.stopCreationCh,
			ts.stopCreationChOnce,
			ts.osSig,
			ts.irsaFargateTester.Create,
		); err != nil {
			return err
		}
	}

	if ts.cfg.IsEnabledAddOnWordpress() {
		if ts.wordPressTester == nil {
			return errors.New("ts.wordPressTester == nil when AddOnWordpress.Enable == true")
		}
		fmt.Printf("\n*********************************\n")
		fmt.Printf("wordPressTester.Create (%q, \"%s --namespace=%s get all\")\n", ts.cfg.ConfigPath, ts.cfg.KubectlCommand(), ts.cfg.AddOnWordpress.Namespace)
		if err := catchInterrupt(
			ts.lg,
			ts.stopCreationCh,
			ts.stopCreationChOnce,
			ts.osSig,
			ts.wordPressTester.Create,
		); err != nil {
			return err
		}
	}

	if ts.cfg.IsEnabledAddOnJupyterHub() {
		if ts.jupyterHubTester == nil {
			return errors.New("ts.jupyterHubTester == nil when AddOnJupyterHub.Enable == true")
		}
		fmt.Printf("\n*********************************\n")
		fmt.Printf("jupyterHubTester.Create (%q, \"%s --namespace=%s get all\")\n", ts.cfg.ConfigPath, ts.cfg.KubectlCommand(), ts.cfg.AddOnJupyterHub.Namespace)
		if err := catchInterrupt(
			ts.lg,
			ts.stopCreationCh,
			ts.stopCreationChOnce,
			ts.osSig,
			ts.jupyterHubTester.Create,
		); err != nil {
			return err
		}
	}

	if ts.cfg.IsEnabledAddOnKubeflow() {
		if ts.kubeflowTester == nil {
			return errors.New("ts.kubeflowTester == nil when AddOnKubeflow.Enable == true")
		}
		fmt.Printf("\n*********************************\n")
		fmt.Printf("kubeflowTester.Create (%q, \"%s --namespace=kube-system get all\")\n", ts.cfg.ConfigPath, ts.cfg.KubectlCommand())
		if err := catchInterrupt(
			ts.lg,
			ts.stopCreationCh,
			ts.stopCreationChOnce,
			ts.osSig,
			ts.kubeflowTester.Create,
		); err != nil {
			return err
		}
	}

	if ts.cfg.IsEnabledAddOnHollowNodesLocal() {
		if ts.hollowNodesLocalTester == nil {
			return errors.New("ts.hollowNodesLocalTester == nil when AddOnHollowNodesLocal.Enable == true")
		}
		fmt.Printf("\n*********************************\n")
		fmt.Printf("hollowNodesLocalTester.Create (%q, \"%s get all\")\n", ts.cfg.ConfigPath, ts.cfg.KubectlCommand())
		if err := catchInterrupt(
			ts.lg,
			ts.stopCreationCh,
			ts.stopCreationChOnce,
			ts.osSig,
			ts.hollowNodesLocalTester.Create,
		); err != nil {
			return err
		}
	}
	if ts.cfg.IsEnabledAddOnHollowNodesRemote() {
		if ts.hollowNodesRemoteTester == nil {
			return errors.New("ts.hollowNodesRemoteTester == nil when AddOnHollowNodesRemote.Enable == true")
		}
		fmt.Printf("\n*********************************\n")
		fmt.Printf("hollowNodesRemoteTester.Create (%q, \"%s --namespace=%s get all\")\n", ts.cfg.ConfigPath, ts.cfg.KubectlCommand(), ts.cfg.AddOnHollowNodesRemote.Namespace)
		if err := catchInterrupt(
			ts.lg,
			ts.stopCreationCh,
			ts.stopCreationChOnce,
			ts.osSig,
			ts.hollowNodesRemoteTester.Create,
		); err != nil {
			return err
		}
	}

	if ts.cfg.IsEnabledAddOnClusterLoaderLocal() {
		if ts.clusterLoaderLocalTester == nil {
			return errors.New("ts.clusterLoaderLocalTester == nil when AddOnClusterLoader.Enable == true")
		}
		fmt.Printf("\n*********************************\n")
		fmt.Printf("clusterLoaderLocalTester.Create (%q, \"%s get all\")\n", ts.cfg.ConfigPath, ts.cfg.KubectlCommand())
		if err := catchInterrupt(
			ts.lg,
			ts.stopCreationCh,
			ts.stopCreationChOnce,
			ts.osSig,
			ts.clusterLoaderLocalTester.Create,
		); err != nil {
			return err
		}
	}
	if ts.cfg.IsEnabledAddOnClusterLoaderRemote() {
		if ts.clusterLoaderRemoteTester == nil {
			return errors.New("ts.clusterLoaderRemoteTester == nil when AddOnClusterLoader.Enable == true")
		}
		fmt.Printf("\n*********************************\n")
		fmt.Printf("clusterLoaderRemoteTester.Create (%q, \"%s --namespace=%s get all\")\n", ts.cfg.ConfigPath, ts.cfg.KubectlCommand(), ts.cfg.AddOnClusterLoaderRemote.Namespace)
		if err := catchInterrupt(
			ts.lg,
			ts.stopCreationCh,
			ts.stopCreationChOnce,
			ts.osSig,
			ts.clusterLoaderRemoteTester.Create,
		); err != nil {
			return err
		}
	}

	if ts.cfg.IsEnabledAddOnStresserLocal() {
		if ts.stresserLocalTester == nil {
			return errors.New("ts.stresserLocalTester == nil when AddOnStresserLocal.Enable == true")
		}
		fmt.Printf("\n*********************************\n")
		fmt.Printf("stresserLocalTester.Create (%q, \"%s get all\")\n", ts.cfg.ConfigPath, ts.cfg.KubectlCommand())
		if err := catchInterrupt(
			ts.lg,
			ts.stopCreationCh,
			ts.stopCreationChOnce,
			ts.osSig,
			ts.stresserLocalTester.Create,
		); err != nil {
			return err
		}
	}
	if ts.cfg.IsEnabledAddOnStresserRemote() {
		if ts.stresserRemoteTester == nil {
			return errors.New("ts.stresserRemoteTester == nil when AddOnStresserRemote.Enable == true")
		}
		fmt.Printf("\n*********************************\n")
		fmt.Printf("stresserRemoteTester.Create (%q, \"%s --namespace=%s get all\")\n", ts.cfg.ConfigPath, ts.cfg.KubectlCommand(), ts.cfg.AddOnStresserRemote.Namespace)
		if err := catchInterrupt(
			ts.lg,
			ts.stopCreationCh,
			ts.stopCreationChOnce,
			ts.osSig,
			ts.stresserRemoteTester.Create,
		); err != nil {
			return err
		}
	}

	if ts.cfg.IsEnabledAddOnNodeGroups() && ts.cfg.AddOnNodeGroups.Created && ts.cfg.AddOnNodeGroups.FetchLogs {
		if ts.ngTester == nil {
			return errors.New("ts.ngTester == nil when AddOnNodeGroups.Enable == true")
		}

		fmt.Printf("\n*********************************\n")
		fmt.Printf("ngTester.FetchLogs (%q, %q)\n", ts.cfg.ConfigPath, ts.cfg.KubectlCommand())

		waitDur := 15 * time.Second
		ts.lg.Info("sleeping before ngTester.FetchLogs", zap.Duration("wait", waitDur))
		time.Sleep(waitDur)

		if err := catchInterrupt(
			ts.lg,
			ts.stopCreationCh,
			ts.stopCreationChOnce,
			ts.osSig,
			ts.ngTester.FetchLogs,
		); err != nil {
			return err
		}
	}

	if ts.cfg.IsEnabledAddOnManagedNodeGroups() && ts.cfg.AddOnManagedNodeGroups.Created && ts.cfg.AddOnManagedNodeGroups.FetchLogs {
		if ts.mngTester == nil {
			return errors.New("ts.mngTester == nil when AddOnManagedNodeGroups.Enable == true")
		}

		fmt.Printf("\n*********************************\n")
		fmt.Printf("mngTester.FetchLogs (%q, %q)\n", ts.cfg.ConfigPath, ts.cfg.KubectlCommand())

		waitDur := 15 * time.Second
		ts.lg.Info("sleeping before mngTester.FetchLogs", zap.Duration("wait", waitDur))
		time.Sleep(waitDur)

		if err := catchInterrupt(
			ts.lg,
			ts.stopCreationCh,
			ts.stopCreationChOnce,
			ts.osSig,
			ts.mngTester.FetchLogs,
		); err != nil {
			return err
		}
	}

	if (ts.cfg.IsEnabledAddOnNodeGroups() && ts.cfg.AddOnNodeGroups.Created && ts.cfg.AddOnNodeGroups.FetchLogs) ||
		(ts.cfg.IsEnabledAddOnManagedNodeGroups() && ts.cfg.AddOnManagedNodeGroups.Created && ts.cfg.AddOnManagedNodeGroups.FetchLogs) {

		if ts.cfg.IsEnabledAddOnCSRsRemote() {
			fmt.Printf("\n*********************************\n")
			fmt.Printf("csrsRemoteTester.AggregateResults (%q, \"%s --namespace=%s get all\")\n", ts.cfg.ConfigPath, ts.cfg.KubectlCommand(), ts.cfg.AddOnCSRsRemote.Namespace)
			if err := catchInterrupt(
				ts.lg,
				ts.stopCreationCh,
				ts.stopCreationChOnce,
				ts.osSig,
				ts.csrsRemoteTester.AggregateResults,
			); err != nil {
				return err
			}
		}

		if ts.cfg.IsEnabledAddOnSecretsRemote() {
			fmt.Printf("\n*********************************\n")
			fmt.Printf("secretsRemoteTester.AggregateResults (%q, \"%s --namespace=%s get all\")\n", ts.cfg.ConfigPath, ts.cfg.KubectlCommand(), ts.cfg.AddOnSecretsRemote.Namespace)
			if err := catchInterrupt(
				ts.lg,
				ts.stopCreationCh,
				ts.stopCreationChOnce,
				ts.osSig,
				ts.secretsRemoteTester.AggregateResults,
			); err != nil {
				return err
			}
		}

		if ts.cfg.IsEnabledAddOnIRSA() {
			fmt.Printf("\n*********************************\n")
			fmt.Printf("irsaTester.AggregateResults (%q, \"%s --namespace=%s get all\")\n", ts.cfg.ConfigPath, ts.cfg.KubectlCommand(), ts.cfg.AddOnIRSA.Namespace)
			if err := catchInterrupt(
				ts.lg,
				ts.stopCreationCh,
				ts.stopCreationChOnce,
				ts.osSig,
				ts.irsaTester.AggregateResults,
			); err != nil {
				return err
			}
		}

		if ts.cfg.IsEnabledAddOnConfigMapsRemote() {
			fmt.Printf("\n*********************************\n")
			fmt.Printf("configMapsRemoteTester.AggregateResults (%q, \"%s --namespace=%s get all\")\n", ts.cfg.ConfigPath, ts.cfg.KubectlCommand(), ts.cfg.AddOnConfigMapsRemote.Namespace)
			if err := catchInterrupt(
				ts.lg,
				ts.stopCreationCh,
				ts.stopCreationChOnce,
				ts.osSig,
				ts.configMapsRemoteTester.AggregateResults,
			); err != nil {
				return err
			}
		}

		if ts.cfg.IsEnabledAddOnClusterLoaderRemote() {
			fmt.Printf("\n*********************************\n")
			fmt.Printf("clusterLoaderRemoteTester.AggregateResults (%q, \"%s --namespace=%s get all\")\n", ts.cfg.ConfigPath, ts.cfg.KubectlCommand(), ts.cfg.AddOnClusterLoaderRemote.Namespace)
			if err := catchInterrupt(
				ts.lg,
				ts.stopCreationCh,
				ts.stopCreationChOnce,
				ts.osSig,
				ts.clusterLoaderRemoteTester.AggregateResults,
			); err != nil {
				return err
			}
		}

		if ts.cfg.IsEnabledAddOnStresserRemote() {
			fmt.Printf("\n*********************************\n")
			fmt.Printf("stresserRemoteTester.AggregateResults (%q, \"%s --namespace=%s get all\")\n", ts.cfg.ConfigPath, ts.cfg.KubectlCommand(), ts.cfg.AddOnStresserRemote.Namespace)
			if err := catchInterrupt(
				ts.lg,
				ts.stopCreationCh,
				ts.stopCreationChOnce,
				ts.osSig,
				ts.stresserRemoteTester.AggregateResults,
			); err != nil {
				return err
			}
		}
	}

	fmt.Printf("\n*********************************\n")
	fmt.Printf("checkHealth (%q, %q)\n", ts.cfg.ConfigPath, ts.cfg.KubectlCommand())
	if err := catchInterrupt(
		ts.lg,
		ts.stopCreationCh,
		ts.stopCreationChOnce,
		ts.osSig,
		ts.checkHealth,
	); err != nil {
		return err
	}

	if ts.cfg.CommandAfterCreateAddOns != "" {
		if err := ts.cfg.EvaluateCommandRefs(); err != nil {
			return err
		}
		fmt.Printf("\nrunCommand CommandAfterCreateAddOns (%q)\n", ts.cfg.CommandAfterCreateAddOns)
		out, err := runCommand(ts.lg, ts.cfg.CommandAfterCreateAddOns, ts.cfg.CommandAfterCreateAddOnsTimeout)
		if err != nil {
			err = ioutil.WriteFile(ts.cfg.CommandAfterCreateAddOnsOutputPath, []byte(ts.cfg.CommandAfterCreateAddOns+"\n\n# output\n"+string(out)+"\n\n# error\n"+err.Error()), 0600)
			if err != nil {
				return fmt.Errorf("failed to write file %q (%v)", ts.cfg.CommandAfterCreateAddOnsOutputPath, err)
			}
		} else {
			err = ioutil.WriteFile(ts.cfg.CommandAfterCreateAddOnsOutputPath, []byte(ts.cfg.CommandAfterCreateAddOns+"\n\n# output\n"+string(out)), 0600)
			if err != nil {
				return fmt.Errorf("failed to write file %q (%v)", ts.cfg.CommandAfterCreateAddOnsOutputPath, err)
			}
		}
		fmt.Printf("\nrunCommand output:\n\n%s\n", string(out))
	}

	return ts.cfg.Sync()
}

// Down cancels the cluster creation and destroy the test cluster if any.
// ref. https://pkg.go.dev/k8s.io/test-infra/kubetest2/pkg/types?tab=doc#Deployer
// ref. https://pkg.go.dev/k8s.io/test-infra/kubetest2/pkg/types?tab=doc#Options
func (ts *Tester) Down() error {
	ts.downMu.Lock()
	defer ts.downMu.Unlock()
	return ts.down()
}

func (ts *Tester) down() (err error) {
	fmt.Printf("\n*********************************\n")
	fmt.Printf("DOWN START (%q)\n\n", ts.cfg.ConfigPath)

	now := time.Now()
	ts.lg.Warn("starting Down",
		zap.String("name", ts.cfg.Name),
		zap.String("cluster-arn", ts.cfg.Status.ClusterARN),
	)
	if serr := ts.uploadToS3(); serr != nil {
		ts.lg.Warn("failed to upload artifacts to S3", zap.Error(serr))
	}

	defer func() {
		ts.cfg.Sync()
		if err == nil {
			fmt.Printf("\n*********************************\n")
			fmt.Printf("DOWN DEFER START (%q)\n\n", ts.cfg.ConfigPath)
			fmt.Printf("\n\n😁 😁  :) DOWN SUCCESS\n\n\n")

			ts.lg.Info("successfully finished Down",
				zap.String("started", humanize.RelTime(now, time.Now(), "ago", "from now")),
			)

		} else {

			fmt.Printf("\n*********************************\n")
			fmt.Printf("DOWN DEFER START (%q)\n\n", ts.cfg.ConfigPath)
			fmt.Printf("\n\n🔥 💀 👽 😱 😡 (-_-) DOWN FAIL\n\n\n")

			ts.lg.Info("failed Down",
				zap.Error(err),
				zap.String("started", humanize.RelTime(now, time.Now(), "ago", "from now")),
			)
		}
	}()

	var errs []string

	fmt.Printf("\n*********************************\n")
	fmt.Printf("deleteKeyPair (%q)\n", ts.cfg.ConfigPath)
	if err := ts.deleteKeyPair(); err != nil {
		ts.lg.Warn("failed to delete key pair", zap.Error(err))
		errs = append(errs, err.Error())
	}

	if ts.cfg.IsEnabledAddOnStresserRemote() && ts.cfg.AddOnStresserRemote.Created {
		fmt.Printf("\n*********************************\n")
		fmt.Printf("stresserRemoteTester.Delete (%q)\n", ts.cfg.ConfigPath)
		if err := ts.stresserRemoteTester.Delete(); err != nil {
			ts.lg.Warn("stresserRemoteTester.Delete failed", zap.Error(err))
			errs = append(errs, err.Error())
		} else {
			waitDur := 20 * time.Second
			ts.lg.Info("sleeping after deleting stresserRemoteTester", zap.Duration("wait", waitDur))
			time.Sleep(waitDur)
		}
	}
	if ts.cfg.IsEnabledAddOnStresserLocal() && ts.cfg.AddOnStresserLocal.Created {
		fmt.Printf("\n*********************************\n")
		fmt.Printf("stresserLocalTester.Delete (%q)\n", ts.cfg.ConfigPath)
		if err := ts.stresserLocalTester.Delete(); err != nil {
			ts.lg.Warn("stresserLocalTester.Delete failed", zap.Error(err))
			errs = append(errs, err.Error())
		} else {
			waitDur := 20 * time.Second
			ts.lg.Info("sleeping after deleting stresserLocalTester", zap.Duration("wait", waitDur))
			time.Sleep(waitDur)
		}
	}

	if ts.cfg.IsEnabledAddOnClusterLoaderRemote() && ts.cfg.AddOnClusterLoaderRemote.Created {
		fmt.Printf("\n*********************************\n")
		fmt.Printf("clusterLoaderRemoteTester.Delete (%q)\n", ts.cfg.ConfigPath)
		if err := ts.clusterLoaderRemoteTester.Delete(); err != nil {
			ts.lg.Warn("clusterLoaderRemoteTester.Delete failed", zap.Error(err))
			errs = append(errs, err.Error())
		} else {
			waitDur := 20 * time.Second
			ts.lg.Info("sleeping after deleting clusterLoaderRemoteTester", zap.Duration("wait", waitDur))
			time.Sleep(waitDur)
		}
	}
	if ts.cfg.IsEnabledAddOnClusterLoaderLocal() && ts.cfg.AddOnClusterLoaderLocal.Created {
		fmt.Printf("\n*********************************\n")
		fmt.Printf("clusterLoaderRemoteTester.Delete (%q)\n", ts.cfg.ConfigPath)
		if err := ts.clusterLoaderRemoteTester.Delete(); err != nil {
			ts.lg.Warn("clusterLoaderRemoteTester.Delete failed", zap.Error(err))
			errs = append(errs, err.Error())
		} else {
			waitDur := 20 * time.Second
			ts.lg.Info("sleeping after deleting clusterLoaderRemoteTester", zap.Duration("wait", waitDur))
			time.Sleep(waitDur)
		}
	}

	if ts.cfg.IsEnabledAddOnHollowNodesRemote() && ts.cfg.AddOnHollowNodesRemote.Created {
		fmt.Printf("\n*********************************\n")
		fmt.Printf("hollowNodesRemoteTester.Delete (%q)\n", ts.cfg.ConfigPath)
		if err := ts.hollowNodesRemoteTester.Delete(); err != nil {
			ts.lg.Warn("hollowNodesRemoteTester.Delete failed", zap.Error(err))
			errs = append(errs, err.Error())
		}
	}
	if ts.cfg.IsEnabledAddOnHollowNodesLocal() && ts.cfg.AddOnHollowNodesLocal.Created {
		fmt.Printf("\n*********************************\n")
		fmt.Printf("hollowNodesLocalTester.Delete (%q)\n", ts.cfg.ConfigPath)
		if err := ts.hollowNodesLocalTester.Delete(); err != nil {
			ts.lg.Warn("hollowNodesLocalTester.Delete failed", zap.Error(err))
			errs = append(errs, err.Error())
		}
	}

	if ts.cfg.IsEnabledAddOnKubeflow() && ts.cfg.AddOnKubeflow.Created {
		fmt.Printf("\n*********************************\n")
		fmt.Printf("kubeflowTester.Delete (%q)\n", ts.cfg.ConfigPath)
		if err := ts.kubeflowTester.Delete(); err != nil {
			ts.lg.Warn("kubeflowTester.Delete failed", zap.Error(err))
			errs = append(errs, err.Error())
		}
	}

	if ts.cfg.IsEnabledAddOnJupyterHub() && ts.cfg.AddOnJupyterHub.Created {
		fmt.Printf("\n*********************************\n")
		fmt.Printf("jupyterHubTester.Delete (%q)\n", ts.cfg.ConfigPath)
		if err := ts.jupyterHubTester.Delete(); err != nil {
			ts.lg.Warn("jupyterHubTester.Delete failed", zap.Error(err))
			errs = append(errs, err.Error())
		} else {
			waitDur := 20 * time.Second
			ts.lg.Info("sleeping after deleting jupyterHubTester", zap.Duration("wait", waitDur))
			time.Sleep(waitDur)
		}
	}

	if ts.cfg.IsEnabledAddOnWordpress() && ts.cfg.AddOnWordpress.Created {
		fmt.Printf("\n*********************************\n")
		fmt.Printf("wordPressTester.Delete (%q)\n", ts.cfg.ConfigPath)
		if err := ts.wordPressTester.Delete(); err != nil {
			ts.lg.Warn("wordPressTester.Delete failed", zap.Error(err))
			errs = append(errs, err.Error())
		} else {
			waitDur := 20 * time.Second
			ts.lg.Info("sleeping after deleting wordPressTester", zap.Duration("wait", waitDur))
			time.Sleep(waitDur)
		}
	}

	if ts.cfg.IsEnabledAddOnIRSAFargate() && ts.cfg.AddOnIRSAFargate.Created {
		fmt.Printf("\n*********************************\n")
		fmt.Printf("irsaFargateTester.Delete (%q)\n", ts.cfg.ConfigPath)
		if err := ts.irsaFargateTester.Delete(); err != nil {
			ts.lg.Warn("irsaFargateTester.Delete failed", zap.Error(err))
			errs = append(errs, err.Error())
		}
	}

	if ts.cfg.IsEnabledAddOnIRSA() && ts.cfg.AddOnIRSA.Created {
		fmt.Printf("\n*********************************\n")
		fmt.Printf("irsaTester.Delete (%q)\n", ts.cfg.ConfigPath)
		if err := ts.irsaTester.Delete(); err != nil {
			ts.lg.Warn("irsaTester.Delete failed", zap.Error(err))
			errs = append(errs, err.Error())
		}
	}

	if ts.cfg.IsEnabledAddOnFargate() && ts.cfg.AddOnFargate.Created {
		fmt.Printf("\n*********************************\n")
		fmt.Printf("fargateTester.Delete (%q)\n", ts.cfg.ConfigPath)
		if err := ts.fargateTester.Delete(); err != nil {
			ts.lg.Warn("fargateTester.Delete failed", zap.Error(err))
			errs = append(errs, err.Error())
		}
	}

	if ts.cfg.IsEnabledAddOnSecretsLocal() && ts.cfg.AddOnSecretsLocal.Created {
		fmt.Printf("\n*********************************\n")
		fmt.Printf("secretsLocalTester.Delete (%q)\n", ts.cfg.ConfigPath)
		if err := ts.secretsLocalTester.Delete(); err != nil {
			ts.lg.Warn("secretsLocalTester.Delete failed", zap.Error(err))
			errs = append(errs, err.Error())
		}
	}
	if ts.cfg.IsEnabledAddOnSecretsRemote() && ts.cfg.AddOnSecretsRemote.Created {
		fmt.Printf("\n*********************************\n")
		fmt.Printf("secretsRemoteTester.Delete (%q)\n", ts.cfg.ConfigPath)
		if err := ts.secretsRemoteTester.Delete(); err != nil {
			ts.lg.Warn("secretsRemoteTester.Delete failed", zap.Error(err))
			errs = append(errs, err.Error())
		}
	}

	if ts.cfg.IsEnabledAddOnConfigMapsLocal() && ts.cfg.AddOnConfigMapsLocal.Created {
		fmt.Printf("\n*********************************\n")
		fmt.Printf("configMapsLocalTester.Delete (%q)\n", ts.cfg.ConfigPath)
		if err := ts.configMapsLocalTester.Delete(); err != nil {
			ts.lg.Warn("configMapsLocalTester.Delete failed", zap.Error(err))
			errs = append(errs, err.Error())
		}
	}
	if ts.cfg.IsEnabledAddOnCSRsRemote() && ts.cfg.AddOnCSRsRemote.Created {
		fmt.Printf("\n*********************************\n")
		fmt.Printf("csrsRemoteTester.Delete (%q)\n", ts.cfg.ConfigPath)
		if err := ts.csrsRemoteTester.Delete(); err != nil {
			ts.lg.Warn("csrsRemoteTester.Delete failed", zap.Error(err))
			errs = append(errs, err.Error())
		}
	}

	if ts.cfg.IsEnabledAddOnCronJobs() && ts.cfg.AddOnCronJobs.Created {
		fmt.Printf("\n*********************************\n")
		fmt.Printf("cronJobsTester.Delete (%q)\n", ts.cfg.ConfigPath)
		if err := ts.cronJobsTester.Delete(); err != nil {
			ts.lg.Warn("cronJobsTester.Delete failed", zap.Error(err))
			errs = append(errs, err.Error())
		}
	}

	if ts.cfg.IsEnabledAddOnJobsEcho() && ts.cfg.AddOnJobsEcho.Created {
		fmt.Printf("\n*********************************\n")
		fmt.Printf("jobsEchoTester.Delete (%q)\n", ts.cfg.ConfigPath)
		if err := ts.jobsEchoTester.Delete(); err != nil {
			ts.lg.Warn("jobsEchoTester.Delete failed", zap.Error(err))
			errs = append(errs, err.Error())
		}
	}

	if ts.cfg.IsEnabledAddOnJobsPi() && ts.cfg.AddOnJobsPi.Created {
		fmt.Printf("\n*********************************\n")
		fmt.Printf("jobsPiTester.Delete (%q)\n", ts.cfg.ConfigPath)
		if err := ts.jobsPiTester.Delete(); err != nil {
			ts.lg.Warn("jobsPiTester.Delete failed", zap.Error(err))
			errs = append(errs, err.Error())
		}
	}

	if ts.cfg.IsEnabledAddOnALB2048() && ts.cfg.AddOnALB2048.Created {
		fmt.Printf("\n*********************************\n")
		fmt.Printf("alb2048Tester.Delete (%q)\n", ts.cfg.ConfigPath)
		if err := ts.alb2048Tester.Delete(); err != nil {
			ts.lg.Warn("alb2048Tester.Delete failed", zap.Error(err))
			errs = append(errs, err.Error())
		} else {
			waitDur := time.Minute
			ts.lg.Info("sleeping after deleting ALB", zap.Duration("wait", waitDur))
			time.Sleep(waitDur)
		}
	}

	if ts.cfg.IsEnabledAddOnNLBHelloWorld() && ts.cfg.AddOnNLBHelloWorld.Created {
		fmt.Printf("\n*********************************\n")
		fmt.Printf("nlbHelloWorldTester.Delete (%q)\n", ts.cfg.ConfigPath)
		if err := ts.nlbHelloWorldTester.Delete(); err != nil {
			ts.lg.Warn("nlbHelloWorldTester.Delete failed", zap.Error(err))
			errs = append(errs, err.Error())
		} else {
			waitDur := time.Minute
			ts.lg.Info("sleeping after deleting NLB", zap.Duration("wait", waitDur))
			time.Sleep(waitDur)
		}
	}

	if ts.cfg.IsEnabledAddOnPrometheusGrafana() && ts.cfg.AddOnPrometheusGrafana.Created {
		fmt.Printf("\n*********************************\n")
		fmt.Printf("prometheusGrafanaTester.Delete (%q)\n", ts.cfg.ConfigPath)
		if err := ts.prometheusGrafanaTester.Delete(); err != nil {
			ts.lg.Warn("prometheusGrafanaTester.Delete failed", zap.Error(err))
			errs = append(errs, err.Error())
		} else {
			waitDur := 20 * time.Second
			ts.lg.Info("sleeping after deleting prometheusGrafanaTester", zap.Duration("wait", waitDur))
			time.Sleep(waitDur)
		}
	}

	if ts.cfg.IsEnabledAddOnKubernetesDashboard() && ts.cfg.AddOnKubernetesDashboard.Created {
		fmt.Printf("\n*********************************\n")
		fmt.Printf("kubernetesDashboardTester.Delete (%q)\n", ts.cfg.ConfigPath)
		if err := ts.kubernetesDashboardTester.Delete(); err != nil {
			ts.lg.Warn("kubernetesDashboardTester.Delete failed", zap.Error(err))
			errs = append(errs, err.Error())
		}
	}

	if ts.cfg.IsEnabledAddOnAppMesh() && ts.cfg.AddOnAppMesh.Created {
		fmt.Printf("\n*********************************\n")
		fmt.Printf("appMeshTester.Delete (%q)\n", ts.cfg.ConfigPath)
		if err := ts.appMeshTester.Delete(); err != nil {
			ts.lg.Warn("appMeshTester.Delete failed", zap.Error(err))
			errs = append(errs, err.Error())
		}
	}

	if ts.cfg.IsEnabledAddOnCSIEBS() && ts.cfg.AddOnCSIEBS.Created {
		fmt.Printf("\n*********************************\n")
		fmt.Printf("csiEBSTester.Delete (%q)\n", ts.cfg.ConfigPath)
		if err := ts.csiEBSTester.Delete(); err != nil {
			ts.lg.Warn("csiEBSTester.Delete failed", zap.Error(err))
			errs = append(errs, err.Error())
		} else {
			waitDur := 20 * time.Second
			ts.lg.Info("sleeping after deleting csiEBSTester", zap.Duration("wait", waitDur))
			time.Sleep(waitDur)
		}
	}

	if ts.cfg.IsEnabledAddOnConformance() && ts.cfg.AddOnConformance.Created {
		fmt.Printf("\n*********************************\n")
		fmt.Printf("conformanceTester.Delete (%q)\n", ts.cfg.ConfigPath)
		if err := ts.conformanceTester.Delete(); err != nil {
			ts.lg.Warn("conformanceTester.Delete failed", zap.Error(err))
			errs = append(errs, err.Error())
		} else {
			waitDur := 20 * time.Second
			ts.lg.Info("sleeping after deleting conformanceTester", zap.Duration("wait", waitDur))
			time.Sleep(waitDur)
		}
	}

	// NOTE(jaypipes): Wait for a bit here because we asked Kubernetes to
	// delete the NLB hello world and ALB2048 Deployment/Service above, and
	// both of these interact with the underlying Kubernetes AWS cloud provider
	// to clean up the cloud load balancer backing the Service of type
	// LoadBalancer. The calls to delete the Service return immediately
	// (successfully) but the cloud load balancer resources may not have been
	// deleted yet, including the ENIs that were associated with the cloud load
	// balancer. When, later, aws-k8s-tester tries deleting the VPC associated
	// with the test cluster, it will run into permissions issues because the
	// IAM role that created the ENIs associated with the ENIs in subnets
	// associated with the cloud load balancers will no longer exist.
	//
	// https://github.com/aws/aws-k8s-tester/issues/70
	// https://github.com/kubernetes/kubernetes/issues/53451
	// https://github.com/kubernetes/enhancements/blob/master/keps/sig-network/20190423-service-lb-finalizer.md
	if (ts.cfg.IsEnabledAddOnNodeGroups() || ts.cfg.IsEnabledAddOnManagedNodeGroups()) &&
		((ts.cfg.IsEnabledAddOnALB2048() && ts.cfg.AddOnALB2048.Created) ||
			(ts.cfg.IsEnabledAddOnNLBHelloWorld() && ts.cfg.AddOnNLBHelloWorld.Created)) {
		waitDur := 2 * time.Minute
		ts.lg.Info("sleeping after deleting LB", zap.Duration("wait", waitDur))
		time.Sleep(waitDur)
	}

	// following need to be run in order to resolve delete dependency
	// e.g. cluster must be deleted before VPC delete
	if ts.cfg.IsEnabledAddOnManagedNodeGroups() && ts.mngTester != nil {
		fmt.Printf("\n*********************************\n")
		fmt.Printf("mngTester.Delete (%q)\n", ts.cfg.ConfigPath)
		if err := ts.mngTester.Delete(); err != nil {
			ts.lg.Warn("mngTester.Delete failed", zap.Error(err))
			errs = append(errs, err.Error())
		}

		waitDur := 10 * time.Second
		ts.lg.Info("sleeping before cluster deletion", zap.Duration("wait", waitDur))
		time.Sleep(waitDur)
	}

	if ts.cfg.IsEnabledAddOnNodeGroups() && ts.ngTester != nil {
		fmt.Printf("\n*********************************\n")
		fmt.Printf("ngTester.Delete (%q)\n", ts.cfg.ConfigPath)
		if err := ts.ngTester.Delete(); err != nil {
			ts.lg.Warn("ngTester.Delete failed", zap.Error(err))
			errs = append(errs, err.Error())
		}

		waitDur := 10 * time.Second
		ts.lg.Info("sleeping before cluster deletion", zap.Duration("wait", waitDur))
		time.Sleep(waitDur)
	}

	fmt.Printf("\n*********************************\n")
	fmt.Printf("deleteCluster (%q)\n", ts.cfg.ConfigPath)
	if err := ts.deleteCluster(); err != nil {
		ts.lg.Warn("deleteCluster failed", zap.Error(err))
		errs = append(errs, err.Error())
	}

	fmt.Printf("\n*********************************\n")
	fmt.Printf("deleteEncryption (%q)\n", ts.cfg.ConfigPath)
	if err := ts.deleteEncryption(); err != nil {
		ts.lg.Warn("deleteEncryption failed", zap.Error(err))
		errs = append(errs, err.Error())
	}

	fmt.Printf("\n*********************************\n")
	fmt.Printf("deleteClusterRole (%q)\n", ts.cfg.ConfigPath)
	if err := ts.deleteClusterRole(); err != nil {
		ts.lg.Warn("deleteClusterRole failed", zap.Error(err))
		errs = append(errs, err.Error())
	}

	if ts.cfg.Parameters.VPCCreate { // VPC was created
		waitDur := 30 * time.Second
		ts.lg.Info("sleeping before VPC deletion", zap.Duration("wait", waitDur))
		time.Sleep(waitDur)
	}

	fmt.Printf("\n*********************************\n")
	fmt.Printf("deleteVPC (%q)\n", ts.cfg.ConfigPath)
	if err := ts.deleteVPC(); err != nil {
		ts.lg.Warn("deleteVPC failed", zap.Error(err))
		errs = append(errs, err.Error())
	}

	fmt.Printf("\n*********************************\n")
	fmt.Printf("deleteS3 (%q)\n", ts.cfg.ConfigPath)
	if err := ts.deleteS3(); err != nil {
		ts.lg.Warn("deleteS3 failed", zap.Error(err))
		errs = append(errs, err.Error())
	}

	if len(errs) > 0 {
		return errors.New(strings.Join(errs, ", "))
	}
	return ts.cfg.Sync()
}

// IsUp should return true if a test cluster is successfully provisioned.
// ref. https://pkg.go.dev/k8s.io/test-infra/kubetest2/pkg/types?tab=doc#Deployer
// ref. https://pkg.go.dev/k8s.io/test-infra/kubetest2/pkg/types?tab=doc#Options
func (ts *Tester) IsUp() (up bool, err error) {
	if ts.cfg == nil {
		return false, nil
	}
	if ts.cfg.Status == nil {
		return false, nil
	}
	if !ts.cfg.Status.Up {
		return false, nil
	}
	return true, ts.checkHealth()
}

// DumpClusterLogs should export logs from the cluster. It may be called
// multiple times. Options for this should come from New(...)
// ref. https://pkg.go.dev/k8s.io/test-infra/kubetest2/pkg/types?tab=doc#Deployer
// ref. https://pkg.go.dev/k8s.io/test-infra/kubetest2/pkg/types?tab=doc#Options
func (ts *Tester) DumpClusterLogs() error {
	if ts.cfg.IsEnabledAddOnNodeGroups() {
		if err := ts.ngTester.FetchLogs(); err != nil {
			return err
		}
	}
	if ts.cfg.IsEnabledAddOnManagedNodeGroups() {
		return ts.mngTester.FetchLogs()
	}
	return nil
}

// DownloadClusterLogs dumps all logs to artifact directory.
// Let default kubetest log dumper handle all artifact uploads.
// See https://github.com/kubernetes/test-infra/pull/9811/files#r225776067.
// ref. https://pkg.go.dev/k8s.io/test-infra/kubetest2/pkg/types?tab=doc#Deployer
// ref. https://pkg.go.dev/k8s.io/test-infra/kubetest2/pkg/types?tab=doc#Options
func (ts *Tester) DownloadClusterLogs(artifactDir, _ string) error {
	if ts.cfg.IsEnabledAddOnNodeGroups() {
		if err := ts.mngTester.DownloadClusterLogs(artifactDir); err != nil {
			return err
		}
	}
	if ts.cfg.IsEnabledAddOnManagedNodeGroups() {
		return ts.ngTester.DownloadClusterLogs(artifactDir)
	}
	return nil
}

// Build should build kubernetes and package it in whatever format
// the deployer consumes.
// ref. https://pkg.go.dev/k8s.io/test-infra/kubetest2/pkg/types?tab=doc#Deployer
// ref. https://pkg.go.dev/k8s.io/test-infra/kubetest2/pkg/types?tab=doc#Options
func (ts *Tester) Build() error {
	// no-op
	return nil
}

// LoadConfig reloads configuration from disk to read the latest
// cluster configuration and its states.
// It's either reloaded from disk or returned from embedded EKS deployer.
// ref. https://pkg.go.dev/k8s.io/test-infra/kubetest2/pkg/types?tab=doc#Deployer
// ref. https://pkg.go.dev/k8s.io/test-infra/kubetest2/pkg/types?tab=doc#Options
func (ts *Tester) LoadConfig() (eksconfig.Config, error) {
	return *ts.cfg, nil
}

// KubernetesClientSet returns Kubernetes Go client.
// ref. https://pkg.go.dev/k8s.io/test-infra/kubetest2/pkg/types?tab=doc#Deployer
// ref. https://pkg.go.dev/k8s.io/test-infra/kubetest2/pkg/types?tab=doc#Options
func (ts *Tester) KubernetesClientSet() *kubernetes.Clientset {
	return ts.k8sClient.KubernetesClientSet()
}

// Kubeconfig returns a path to a kubeconfig file for the cluster.
// ref. https://pkg.go.dev/k8s.io/test-infra/kubetest2/pkg/types?tab=doc#Deployer
// ref. https://pkg.go.dev/k8s.io/test-infra/kubetest2/pkg/types?tab=doc#Options
func (ts *Tester) Kubeconfig() (string, error) {
	if ts.cfg == nil {
		return "", errors.New("empty tester object")
	}
	return ts.cfg.KubeConfigPath, nil
}

// Provider returns the kubernetes provider for legacy deployers.
// ref. https://pkg.go.dev/k8s.io/test-infra/kubetest2/pkg/types?tab=doc#Deployer
// ref. https://pkg.go.dev/k8s.io/test-infra/kubetest2/pkg/types?tab=doc#Options
func (ts *Tester) Provider() string {
	return "eks"
}

// HelpRequested true, help text will be shown to the user after instancing
// the deployer and tester.
// ref. https://pkg.go.dev/k8s.io/test-infra/kubetest2/pkg/types?tab=doc#Options
func (ts *Tester) HelpRequested() bool {
	return false
}

// ShouldBuild true, kubetest2 will be calling deployer.Build.
// ref. https://pkg.go.dev/k8s.io/test-infra/kubetest2/pkg/types?tab=doc#Options
func (ts *Tester) ShouldBuild() bool {
	return false
}

// ShouldUp true, kubetest2 will be calling deployer.Up.
// ref. https://pkg.go.dev/k8s.io/test-infra/kubetest2/pkg/types?tab=doc#Options
func (ts *Tester) ShouldUp() bool {
	if ts.cfg == nil {
		return false
	}
	return !ts.cfg.Status.Up
}

// ShouldDown true, kubetest2 will be calling deployer.Down.
// ref. https://pkg.go.dev/k8s.io/test-infra/kubetest2/pkg/types?tab=doc#Options
func (ts *Tester) ShouldDown() bool {
	if ts.cfg == nil {
		return false
	}
	return ts.cfg.Status.Up
}

// ShouldTest true, kubetest2 will be calling tester.Test.
// ref. https://pkg.go.dev/k8s.io/test-infra/kubetest2/pkg/types?tab=doc#Options
func (ts *Tester) ShouldTest() bool {
	if ts.cfg == nil {
		return false
	}
	return ts.cfg.Status.Up
}

// ArtifactsDir returns the path to the directory where artifacts should be written
// (including metadata files like junit_runner.xml).
// ref. https://pkg.go.dev/k8s.io/test-infra/kubetest2/pkg/types?tab=doc#Options
func (ts *Tester) ArtifactsDir() string {
	if ts.cfg == nil {
		return ""
	}
	if ts.cfg.IsEnabledAddOnManagedNodeGroups() {
		return ts.cfg.AddOnManagedNodeGroups.LogsDir
	}
	if ts.cfg.IsEnabledAddOnNodeGroups() {
		return ts.cfg.AddOnNodeGroups.LogsDir
	}
	return ""
}
