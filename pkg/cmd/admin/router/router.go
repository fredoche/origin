package router

import (
	"fmt"
	"io"
	"math/rand"
	"os"
	"path"
	"strconv"
	"strings"

	"github.com/golang/glog"
	"github.com/spf13/cobra"

	kapi "k8s.io/kubernetes/pkg/api"
	"k8s.io/kubernetes/pkg/api/errors"
	kclient "k8s.io/kubernetes/pkg/client/unversioned"
	kclientcmd "k8s.io/kubernetes/pkg/client/unversioned/clientcmd"
	kcmdutil "k8s.io/kubernetes/pkg/kubectl/cmd/util"
	"k8s.io/kubernetes/pkg/runtime"
	"k8s.io/kubernetes/pkg/serviceaccount"
	"k8s.io/kubernetes/pkg/util/intstr"

	authapi "github.com/openshift/origin/pkg/authorization/api"
	"github.com/openshift/origin/pkg/cmd/server/bootstrappolicy"
	cmdutil "github.com/openshift/origin/pkg/cmd/util"
	"github.com/openshift/origin/pkg/cmd/util/clientcmd"
	"github.com/openshift/origin/pkg/cmd/util/variable"
	configcmd "github.com/openshift/origin/pkg/config/cmd"
	deployapi "github.com/openshift/origin/pkg/deploy/api"
	"github.com/openshift/origin/pkg/generate/app"
	"github.com/openshift/origin/pkg/security/admission"
	fileutil "github.com/openshift/origin/pkg/util/file"
)

const (
	routerLong = `
Install or configure a router

This command helps to setup a router to take edge traffic and balance it to
your application. With no arguments, the command will check for an existing router
service called 'router' and create one if it does not exist. If you want to test whether
a router has already been created add the --dry-run flag and the command will exit with
1 if the registry does not exist.

If a router does not exist with the given name, this command will
create a deployment configuration and service that will run the router. If you are
running your router in production, you should pass --replicas=2 or higher to ensure
you have failover protection.`

	routerExample = `  # Check the default router ("router")
  $ %[1]s %[2]s --dry-run

  # See what the router would look like if created
  $ %[1]s %[2]s -o json --credentials=/path/to/openshift-router.kubeconfig --service-account=myserviceaccount

  # Create a router if it does not exist
  $ %[1]s %[2]s router-west --credentials=/path/to/openshift-router.kubeconfig --service-account=myserviceaccount --replicas=2

  # Use a different router image and see the router configuration
  $ %[1]s %[2]s region-west -o yaml --credentials=/path/to/openshift-router.kubeconfig --service-account=myserviceaccount --images=myrepo/somerouter:mytag

  # Run the router with a hint to the underlying implementation to _not_ expose statistics.
  $ %[1]s %[2]s router-west --credentials=/path/to/openshift-router.kubeconfig --service-account=myserviceaccount --stats-port=0
  `

	secretsVolumeName = "secret-volume"
	secretsPath       = "/etc/secret-volume"

	// this is the official private certificate path on Red Hat distros, and is at least structurally more
	// correct than ubuntu based distributions which don't distinguish between public and private certs.
	// Since Origin is CentOS based this is more likely to work.  Ubuntu images should symlink this directory
	// into /etc/ssl/certs to be compatible.
	defaultCertificateDir = "/etc/pki/tls/private"

	privkeySecretName = "external-host-private-key-secret"
	privkeyVolumeName = "external-host-private-key-volume"
	privkeyName       = "router.pem"
	privkeyPath       = secretsPath + "/" + privkeyName
)

var defaultCertificatePath = path.Join(defaultCertificateDir, "tls.crt")

// RouterConfig contains the configuration parameters necessary to
// launch a router, including general parameters, type of router, and
// type-specific parameters.
type RouterConfig struct {
	// Name is the router name, set as an argument
	Name string

	// Type is the router type, which determines which plugin to use (f5
	// or template).
	Type string

	// Subdomain is the subdomain served by this router. This may not be
	// accepted by all routers.
	Subdomain string

	// ImageTemplate specifies the image from which the router will be created.
	ImageTemplate variable.ImageTemplate

	// Ports specifies the container ports for the router.
	Ports string

	// Replicas specifies the initial replica count for the router.
	Replicas int

	// Labels specifies the label or labels that will be assigned to the router
	// pod.
	Labels string

	// DryRun specifies that the router command should not launch a router but
	// should instead exit with code 1 to indicate if a router is already running
	// or code 0 otherwise.
	DryRun bool

	// SecretsAsEnv sets the credentials as env vars, instead of secrets.
	SecretsAsEnv bool

	// Credentials specifies the path to a .kubeconfig file with the credentials
	// with which the router may contact the master.
	Credentials string

	// DefaultCertificate holds the certificate that will be used if no more
	// specific certificate is found.  This is typically a wildcard certificate.
	DefaultCertificate string

	// Selector specifies a label or set of labels that determines the nodes on
	// which the router pod can be scheduled.
	Selector string

	// StatsPort specifies a port at which the router can provide statistics.
	StatsPort int

	// StatsPassword specifies a password required to authenticate connections to
	// the statistics port.
	StatsPassword string

	// StatsUsername specifies a username required to authenticate connections to
	// the statistics port.
	StatsUsername string

	// HostNetwork specifies whether to configure the router pod to use the host's
	// network namespace or the container's.
	HostNetwork bool

	// ServiceAccount specifies the service account under which the router will
	// run.
	ServiceAccount string

	// ExternalHost specifies the hostname or IP address of an external host for
	// router plugins that integrate with an external load balancer (such as f5).
	ExternalHost string

	// ExternalHostUsername specifies the username for authenticating with the
	// external host.
	ExternalHostUsername string

	// ExternalHostPassword specifies the password for authenticating with the
	// external host.
	ExternalHostPassword string

	// ExternalHostHttpVserver specifies the virtual server for HTTP connections.
	ExternalHostHttpVserver string

	// ExternalHostHttpsVserver specifies the virtual server for HTTPS connections.
	ExternalHostHttpsVserver string

	// ExternalHostPrivateKey specifies an SSH private key for authenticating with
	// the external host.
	ExternalHostPrivateKey string

	// ExternalHostInsecure specifies that the router should skip strict
	// certificate verification when connecting to the external host.
	ExternalHostInsecure bool

	// ExternalHostPartitionPath specifies the partition path to use.
	// This is used by some routers to create access access control
	// boundaries for users and applications.
	ExternalHostPartitionPath string

	// ExposeMetrics is a hint on whether to expose metrics.
	ExposeMetrics bool

	// MetricsImage is the image to run a sidecar container with in the router
	// pod.
	MetricsImage string
}

var errExit = fmt.Errorf("exit")

const (
	defaultLabel = "router=<name>"

	// Default port numbers to expose and bind/listen on.
	defaultPorts = "80:80,443:443"

	// Default stats and healthz port.
	defaultStatsPort   = 1936
	defaultHealthzPort = defaultStatsPort
)

// NewCmdRouter implements the OpenShift CLI router command.
func NewCmdRouter(f *clientcmd.Factory, parentName, name string, out io.Writer) *cobra.Command {
	cfg := &RouterConfig{
		Name:          "router",
		ImageTemplate: variable.NewDefaultImageTemplate(),

		ServiceAccount: "router",

		Labels:   defaultLabel,
		Ports:    defaultPorts,
		Replicas: 1,

		StatsUsername: "admin",
		StatsPort:     defaultStatsPort,
		HostNetwork:   true,
	}

	cmd := &cobra.Command{
		Use:     fmt.Sprintf("%s [NAME]", name),
		Short:   "Install a router",
		Long:    routerLong,
		Example: fmt.Sprintf(routerExample, parentName, name),
		Run: func(cmd *cobra.Command, args []string) {
			err := RunCmdRouter(f, cmd, out, cfg, args)
			if err != errExit {
				kcmdutil.CheckErr(err)
			} else {
				os.Exit(1)
			}
		},
	}

	cmd.Flags().StringVar(&cfg.Type, "type", "haproxy-router", "The type of router to use - if you specify --images this flag may be ignored.")
	cmd.Flags().StringVar(&cfg.Subdomain, "subdomain", "", "The template for the route subdomain exposed by this router, used for routes that are not externally specified. E.g. '${name}-${namespace}.apps.mycompany.com'")
	cmd.Flags().StringVar(&cfg.ImageTemplate.Format, "images", cfg.ImageTemplate.Format, "The image to base this router on - ${component} will be replaced with --type")
	cmd.Flags().BoolVar(&cfg.ImageTemplate.Latest, "latest-images", cfg.ImageTemplate.Latest, "If true, attempt to use the latest images for the router instead of the latest release.")
	cmd.Flags().StringVar(&cfg.Ports, "ports", cfg.Ports, "A comma delimited list of ports or port pairs to expose on the router pod. The default is set for HAProxy. Port pairs are applied to the service.")
	cmd.Flags().IntVar(&cfg.Replicas, "replicas", cfg.Replicas, "The replication factor of the router; commonly 2 when high availability is desired.")
	cmd.Flags().StringVar(&cfg.Labels, "labels", cfg.Labels, "A set of labels to uniquely identify the router and its components.")
	cmd.Flags().BoolVar(&cfg.DryRun, "dry-run", cfg.DryRun, "Exit with code 1 if the specified router does not exist.")
	cmd.Flags().BoolVar(&cfg.SecretsAsEnv, "secrets-as-env", cfg.SecretsAsEnv, "Use environment variables for master secrets.")
	cmd.Flags().Bool("create", false, "deprecated; this is now the default behavior")
	cmd.Flags().StringVar(&cfg.Credentials, "credentials", "", "Path to a .kubeconfig file that will contain the credentials the router should use to contact the master.")
	cmd.Flags().StringVar(&cfg.DefaultCertificate, "default-cert", cfg.DefaultCertificate, "Optional path to a certificate file that be used as the default certificate.  The file should contain the cert, key, and any CA certs necessary for the router to serve the certificate.")
	cmd.Flags().StringVar(&cfg.Selector, "selector", cfg.Selector, "Selector used to filter nodes on deployment. Used to run routers on a specific set of nodes.")
	cmd.Flags().StringVar(&cfg.ServiceAccount, "service-account", cfg.ServiceAccount, "Name of the service account to use to run the router pod.")
	cmd.Flags().IntVar(&cfg.StatsPort, "stats-port", cfg.StatsPort, "If the underlying router implementation can provide statistics this is a hint to expose it on this port. Specify 0 if you want to turn off exposing the statistics.")
	cmd.Flags().StringVar(&cfg.StatsPassword, "stats-password", cfg.StatsPassword, "If the underlying router implementation can provide statistics this is the requested password for auth.  If not set a password will be generated.")
	cmd.Flags().StringVar(&cfg.StatsUsername, "stats-user", cfg.StatsUsername, "If the underlying router implementation can provide statistics this is the requested username for auth.")
	cmd.Flags().BoolVar(&cfg.ExposeMetrics, "expose-metrics", cfg.ExposeMetrics, "This is a hint to run an extra container in the pod to expose metrics - the image will either be set depending on the router implementation or provided with --metrics-image.")
	cmd.Flags().StringVar(&cfg.MetricsImage, "metrics-image", cfg.MetricsImage, "If --expose-metrics is specified this is the image to use to run a sidecar container in the pod exposing metrics. If not set and --expose-metrics is true the image will depend on router implementation.")
	cmd.Flags().BoolVar(&cfg.HostNetwork, "host-network", cfg.HostNetwork, "If true (the default), then use host networking rather than using a separate container network stack.")
	cmd.Flags().StringVar(&cfg.ExternalHost, "external-host", cfg.ExternalHost, "If the underlying router implementation connects with an external host, this is the external host's hostname.")
	cmd.Flags().StringVar(&cfg.ExternalHostUsername, "external-host-username", cfg.ExternalHostUsername, "If the underlying router implementation connects with an external host, this is the username for authenticating with the external host.")
	cmd.Flags().StringVar(&cfg.ExternalHostPassword, "external-host-password", cfg.ExternalHostPassword, "If the underlying router implementation connects with an external host, this is the password for authenticating with the external host.")
	cmd.Flags().StringVar(&cfg.ExternalHostHttpVserver, "external-host-http-vserver", cfg.ExternalHostHttpVserver, "If the underlying router implementation uses virtual servers, this is the name of the virtual server for HTTP connections.")
	cmd.Flags().StringVar(&cfg.ExternalHostHttpsVserver, "external-host-https-vserver", cfg.ExternalHostHttpsVserver, "If the underlying router implementation uses virtual servers, this is the name of the virtual server for HTTPS connections.")
	cmd.Flags().StringVar(&cfg.ExternalHostPrivateKey, "external-host-private-key", cfg.ExternalHostPrivateKey, "If the underlying router implementation requires an SSH private key, this is the path to the private key file.")
	cmd.Flags().BoolVar(&cfg.ExternalHostInsecure, "external-host-insecure", cfg.ExternalHostInsecure, "If the underlying router implementation connects with an external host over a secure connection, this causes the router to skip strict certificate verification with the external host.")
	cmd.Flags().StringVar(&cfg.ExternalHostPartitionPath, "external-host-partition-path", cfg.ExternalHostPartitionPath, "If the underlying router implementation uses partitions for control boundaries, this is the path to use for that partition.")

	cmd.MarkFlagFilename("credentials", "kubeconfig")

	kcmdutil.AddPrinterFlags(cmd)

	return cmd
}

// generateSecretsConfig generates any Secret and Volume objects, such
// as SSH private keys, that are necessary for the router container.
func generateSecretsConfig(cfg *RouterConfig, kClient *kclient.Client,
	namespace string, defaultCert []byte) ([]*kapi.Secret, []kapi.Volume, []kapi.VolumeMount,
	error) {
	var secrets []*kapi.Secret
	var volumes []kapi.Volume
	var mounts []kapi.VolumeMount

	if len(cfg.ExternalHostPrivateKey) != 0 {
		privkeyData, err := fileutil.LoadData(cfg.ExternalHostPrivateKey)
		if err != nil {
			return secrets, volumes, mounts, fmt.Errorf("error reading private key for external host: %v", err)
		}

		secret := &kapi.Secret{
			ObjectMeta: kapi.ObjectMeta{
				Name: privkeySecretName,
			},
			Data: map[string][]byte{privkeyName: privkeyData},
		}
		secrets = append(secrets, secret)

		volume := kapi.Volume{
			Name: secretsVolumeName,
			VolumeSource: kapi.VolumeSource{
				Secret: &kapi.SecretVolumeSource{
					SecretName: privkeySecretName,
				},
			},
		}
		volumes = append(volumes, volume)

		mount := kapi.VolumeMount{
			Name:      secretsVolumeName,
			ReadOnly:  true,
			MountPath: secretsPath,
		}
		mounts = append(mounts, mount)
	}

	if len(defaultCert) > 0 {
		keys, err := cmdutil.PrivateKeysFromPEM(defaultCert)
		if err != nil {
			return nil, nil, nil, err
		}
		if len(keys) == 0 {
			return nil, nil, nil, fmt.Errorf("the default cert must contain a private key")
		}
		secret := &kapi.Secret{
			ObjectMeta: kapi.ObjectMeta{
				Name: fmt.Sprintf("%s-certs", cfg.Name),
			},
			Type: kapi.SecretTypeTLS,
			Data: map[string][]byte{
				kapi.TLSCertKey:       defaultCert,
				kapi.TLSPrivateKeyKey: keys,
			},
		}
		secrets = append(secrets, secret)
		volume := kapi.Volume{
			Name: "server-certificate",
			VolumeSource: kapi.VolumeSource{
				Secret: &kapi.SecretVolumeSource{
					SecretName: secret.Name,
				},
			},
		}
		volumes = append(volumes, volume)

		mount := kapi.VolumeMount{
			Name:      volume.Name,
			ReadOnly:  true,
			MountPath: defaultCertificateDir,
		}
		mounts = append(mounts, mount)
	}

	return secrets, volumes, mounts, nil
}

func generateProbeConfigForRouter(cfg *RouterConfig, ports []kapi.ContainerPort) *kapi.Probe {
	var probe *kapi.Probe

	if cfg.Type == "haproxy-router" {
		probe = &kapi.Probe{}
		healthzPort := defaultHealthzPort
		if cfg.StatsPort > 0 {
			healthzPort = cfg.StatsPort
		}

		probe.Handler.HTTPGet = &kapi.HTTPGetAction{
			Path: "/healthz",
			Port: intstr.IntOrString{
				Type:   intstr.Int,
				IntVal: int32(healthzPort),
			},
		}

		// Workaround for misconfigured environments where the Node's InternalIP is
		// physically present on the Node.  In those environments the probes will
		// fail unless a host firewall port is opened
		if cfg.HostNetwork {
			probe.Handler.HTTPGet.Host = "localhost"
		}
	}

	return probe
}

func generateLivenessProbeConfig(cfg *RouterConfig, ports []kapi.ContainerPort) *kapi.Probe {
	probe := generateProbeConfigForRouter(cfg, ports)
	if probe != nil {
		probe.InitialDelaySeconds = 10
	}
	return probe
}

func generateReadinessProbeConfig(cfg *RouterConfig, ports []kapi.ContainerPort) *kapi.Probe {
	probe := generateProbeConfigForRouter(cfg, ports)
	if probe != nil {
		probe.InitialDelaySeconds = 10
	}
	return probe
}

func generateMetricsExporterContainer(cfg *RouterConfig, env app.Environment) *kapi.Container {
	containerName := "metrics-exporter"
	if len(cfg.MetricsImage) > 0 {
		return &kapi.Container{
			Name:  containerName,
			Image: cfg.MetricsImage,
			Env:   env.List(),
		}
	}
	switch cfg.Type {
	case "haproxy-router":
		return &kapi.Container{
			Name:  containerName,
			Image: "prom/haproxy-exporter:latest",
			Env:   env.List(),
			Args: []string{
				fmt.Sprintf("-haproxy.scrape-uri=http://$(STATS_USERNAME):$(STATS_PASSWORD)@localhost:$(STATS_PORT)/haproxy?stats;csv"),
			},
			Ports: []kapi.ContainerPort{
				{
					Name:          "http",
					ContainerPort: 9101,
				},
			},
		}
	default:
		return nil
	}
}

// RunCmdRouter contains all the necessary functionality for the
// OpenShift CLI router command.
func RunCmdRouter(f *clientcmd.Factory, cmd *cobra.Command, out io.Writer, cfg *RouterConfig, args []string) error {
	switch len(args) {
	case 0:
		// uses default value
	case 1:
		cfg.Name = args[0]
	default:
		return kcmdutil.UsageError(cmd, "You may pass zero or one arguments to provide a name for the router")
	}
	name := cfg.Name

	if len(cfg.StatsUsername) > 0 {
		if strings.Contains(cfg.StatsUsername, ":") {
			return kcmdutil.UsageError(cmd, "username %s must not contain ':'", cfg.StatsUsername)
		}
	}

	ports, err := app.ContainerPortsFromString(cfg.Ports)
	if err != nil {
		return fmt.Errorf("unable to parse --ports: %v", err)
	}

	// For the host networking case, ensure the ports match. Otherwise, remove host ports
	for i := 0; i < len(ports); i++ {
		if cfg.HostNetwork && ports[i].HostPort != 0 && ports[i].ContainerPort != ports[i].HostPort {
			return fmt.Errorf("when using host networking mode, container port %d and host port %d must be equal", ports[i].ContainerPort, ports[i].HostPort)
		}
	}

	if cfg.StatsPort > 0 {
		port := kapi.ContainerPort{
			Name:          "stats",
			ContainerPort: cfg.StatsPort,
			Protocol:      kapi.ProtocolTCP,
		}
		ports = append(ports, port)
	}

	label := map[string]string{"router": name}
	if cfg.Labels != defaultLabel {
		valid, remove, err := app.LabelsFromSpec(strings.Split(cfg.Labels, ","))
		if err != nil {
			glog.Fatal(err)
		}
		if len(remove) > 0 {
			return kcmdutil.UsageError(cmd, "You may not pass negative labels in %q", cfg.Labels)
		}
		label = valid
	}

	nodeSelector := map[string]string{}
	if len(cfg.Selector) > 0 {
		valid, remove, err := app.LabelsFromSpec(strings.Split(cfg.Selector, ","))
		if err != nil {
			glog.Fatal(err)
		}
		if len(remove) > 0 {
			return kcmdutil.UsageError(cmd, "You may not pass negative labels in selector %q", cfg.Selector)
		}
		nodeSelector = valid
	}

	image := cfg.ImageTemplate.ExpandOrDie(cfg.Type)

	namespace, _, err := f.OpenShiftClientConfig.Namespace()
	if err != nil {
		return fmt.Errorf("error getting client: %v", err)
	}
	_, kClient, err := f.Clients()
	if err != nil {
		return fmt.Errorf("error getting client: %v", err)
	}

	_, output, err := kcmdutil.PrinterForCommand(cmd)
	if err != nil {
		return fmt.Errorf("unable to configure printer: %v", err)
	}

	generate := output
	if !generate {
		_, err = kClient.Services(namespace).Get(name)
		if err != nil {
			if !errors.IsNotFound(err) {
				return fmt.Errorf("can't check for existing router %q: %v", name, err)
			}
			generate = true
		}
	}
	if !generate {
		fmt.Fprintf(out, "Router %q service exists\n", name)
		return nil
	}

	if cfg.DryRun && !output {
		return fmt.Errorf("router %q does not exist (no service)", name)
	}

	if len(cfg.ServiceAccount) == 0 {
		return fmt.Errorf("you must specify a service account for the router with --service-account")
	}

	if err := validateServiceAccount(kClient, namespace, cfg.ServiceAccount, cfg.HostNetwork); err != nil {
		return fmt.Errorf("router could not be created; %v", err)
	}

	// create new router
	secretEnv := app.Environment{}
	switch {
	case len(cfg.Credentials) == 0 && len(cfg.ServiceAccount) == 0:
		return fmt.Errorf("router could not be created; you must specify a .kubeconfig file path containing credentials for connecting the router to the master with --credentials")
	case len(cfg.Credentials) > 0:
		clientConfigLoadingRules := &kclientcmd.ClientConfigLoadingRules{ExplicitPath: cfg.Credentials, Precedence: []string{}}
		credentials, err := clientConfigLoadingRules.Load()
		if err != nil {
			return fmt.Errorf("router could not be created; the provided credentials %q could not be loaded: %v", cfg.Credentials, err)
		}
		config, err := kclientcmd.NewDefaultClientConfig(*credentials, &kclientcmd.ConfigOverrides{}).ClientConfig()
		if err != nil {
			return fmt.Errorf("router could not be created; the provided credentials %q could not be used: %v", cfg.Credentials, err)
		}
		if err := kclient.LoadTLSFiles(config); err != nil {
			return fmt.Errorf("router could not be created; the provided credentials %q could not load certificate info: %v", cfg.Credentials, err)
		}
		insecure := "false"
		if config.Insecure {
			insecure = "true"
		}
		secretEnv.Add(app.Environment{
			"OPENSHIFT_MASTER":    config.Host,
			"OPENSHIFT_CA_DATA":   string(config.CAData),
			"OPENSHIFT_KEY_DATA":  string(config.KeyData),
			"OPENSHIFT_CERT_DATA": string(config.CertData),
			"OPENSHIFT_INSECURE":  insecure,
		})
	}
	createServiceAccount := len(cfg.ServiceAccount) > 0 && len(cfg.Credentials) == 0

	defaultCert, err := fileutil.LoadData(cfg.DefaultCertificate)
	if err != nil {
		return fmt.Errorf("router could not be created; error reading default certificate file: %v", err)
	}

	if len(cfg.StatsPassword) == 0 {
		cfg.StatsPassword = generateStatsPassword()
		if !output {
			fmt.Fprintf(cmd.Out(), "info: password for stats user %s has been set to %s\n", cfg.StatsUsername, cfg.StatsPassword)
		}
	}

	env := app.Environment{
		"ROUTER_SUBDOMAIN":                    cfg.Subdomain,
		"ROUTER_SERVICE_NAME":                 name,
		"ROUTER_SERVICE_NAMESPACE":            namespace,
		"ROUTER_EXTERNAL_HOST_HOSTNAME":       cfg.ExternalHost,
		"ROUTER_EXTERNAL_HOST_USERNAME":       cfg.ExternalHostUsername,
		"ROUTER_EXTERNAL_HOST_PASSWORD":       cfg.ExternalHostPassword,
		"ROUTER_EXTERNAL_HOST_HTTP_VSERVER":   cfg.ExternalHostHttpVserver,
		"ROUTER_EXTERNAL_HOST_HTTPS_VSERVER":  cfg.ExternalHostHttpsVserver,
		"ROUTER_EXTERNAL_HOST_INSECURE":       strconv.FormatBool(cfg.ExternalHostInsecure),
		"ROUTER_EXTERNAL_HOST_PARTITION_PATH": cfg.ExternalHostPartitionPath,
		"ROUTER_EXTERNAL_HOST_PRIVKEY":        privkeyPath,
		"STATS_PORT":                          strconv.Itoa(cfg.StatsPort),
		"STATS_USERNAME":                      cfg.StatsUsername,
		"STATS_PASSWORD":                      cfg.StatsPassword,
	}
	env.Add(secretEnv)
	if len(defaultCert) > 0 {
		if cfg.SecretsAsEnv {
			env.Add(app.Environment{"DEFAULT_CERTIFICATE": string(defaultCert)})
		} else {
			// TODO: make --credentials create secrets and bypass service account
			env.Add(app.Environment{"DEFAULT_CERTIFICATE_PATH": defaultCertificatePath})
		}
	}

	secrets, volumes, mounts, err := generateSecretsConfig(cfg, kClient, namespace, defaultCert)
	if err != nil {
		return fmt.Errorf("router could not be created: %v", err)
	}

	livenessProbe := generateLivenessProbeConfig(cfg, ports)
	readinessProbe := generateReadinessProbeConfig(cfg, ports)

	exposedPorts := make([]kapi.ContainerPort, len(ports))
	copy(exposedPorts, ports)
	for i := range exposedPorts {
		exposedPorts[i].HostPort = 0
	}
	containers := []kapi.Container{
		{
			Name:            "router",
			Image:           image,
			Ports:           exposedPorts,
			Env:             env.List(),
			LivenessProbe:   livenessProbe,
			ReadinessProbe:  readinessProbe,
			ImagePullPolicy: kapi.PullIfNotPresent,
			VolumeMounts:    mounts,
		},
	}

	if cfg.StatsPort > 0 && cfg.ExposeMetrics {
		pc := generateMetricsExporterContainer(cfg, env)
		if pc != nil {
			containers = append(containers, *pc)
		}
	}

	objects := []runtime.Object{}
	for _, s := range secrets {
		objects = append(objects, s)
	}
	if createServiceAccount {
		objects = append(objects,
			&kapi.ServiceAccount{ObjectMeta: kapi.ObjectMeta{Name: cfg.ServiceAccount}},
			&authapi.ClusterRoleBinding{
				ObjectMeta: kapi.ObjectMeta{Name: fmt.Sprintf("router-%s-role", cfg.Name)},
				Subjects: []kapi.ObjectReference{
					{
						Kind:      "ServiceAccount",
						Name:      cfg.ServiceAccount,
						Namespace: namespace,
					},
				},
				RoleRef: kapi.ObjectReference{
					Kind: "ClusterRole",
					Name: "system:router",
				},
			},
		)
	}
	updatePercent := int(-25)
	objects = append(objects, &deployapi.DeploymentConfig{
		ObjectMeta: kapi.ObjectMeta{
			Name:   name,
			Labels: label,
		},
		Spec: deployapi.DeploymentConfigSpec{
			Strategy: deployapi.DeploymentStrategy{
				Type:          deployapi.DeploymentStrategyTypeRolling,
				RollingParams: &deployapi.RollingDeploymentStrategyParams{UpdatePercent: &updatePercent},
			},
			Replicas: cfg.Replicas,
			Selector: label,
			Triggers: []deployapi.DeploymentTriggerPolicy{
				{Type: deployapi.DeploymentTriggerOnConfigChange},
			},
			Template: &kapi.PodTemplateSpec{
				ObjectMeta: kapi.ObjectMeta{Labels: label},
				Spec: kapi.PodSpec{
					SecurityContext: &kapi.PodSecurityContext{
						HostNetwork: cfg.HostNetwork,
					},
					ServiceAccountName: cfg.ServiceAccount,
					NodeSelector:       nodeSelector,
					Containers:         containers,
					Volumes:            volumes,
				},
			},
		},
	})

	objects = app.AddServices(objects, false)
	// set the service port to the provided hostport value
	for i := range objects {
		switch t := objects[i].(type) {
		case *kapi.Service:
			for j, servicePort := range t.Spec.Ports {
				for _, targetPort := range ports {
					if targetPort.ContainerPort == servicePort.Port && targetPort.HostPort != 0 {
						t.Spec.Ports[j].Port = targetPort.HostPort
					}
				}
			}
		}
	}
	// TODO: label all created objects with the same label - router=<name>
	list := &kapi.List{Items: objects}

	if output {
		list.Items, err = cmdutil.ConvertItemsForDisplayFromDefaultCommand(cmd, list.Items)
		if err != nil {
			return err
		}

		if err := f.PrintObject(cmd, list, out); err != nil {
			return fmt.Errorf("unable to print object: %v", err)
		}
		return nil
	}

	mapper, typer := f.Factory.Object()
	bulk := configcmd.Bulk{
		Mapper:            mapper,
		Typer:             typer,
		RESTClientFactory: f.Factory.ClientForMapping,

		After: configcmd.NewPrintNameOrErrorAfter(mapper, kcmdutil.GetFlagString(cmd, "output") == "name", "created", out, cmd.Out()),
	}
	if errs := bulk.Create(list, namespace); len(errs) != 0 {
		return errExit
	}
	return nil
}

// generateStatsPassword creates a random password.
func generateStatsPassword() string {
	allowableChars := []rune("abcdefghijlkmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ1234567890")
	allowableCharLength := len(allowableChars)
	password := []string{}
	for i := 0; i < 10; i++ {
		char := allowableChars[rand.Intn(allowableCharLength)]
		password = append(password, string(char))
	}
	return strings.Join(password, "")
}

func validateServiceAccount(client *kclient.Client, ns string, serviceAccount string, hostNetwork bool) error {
	if !hostNetwork {
		return nil
	}

	// get cluster sccs
	sccList, err := client.SecurityContextConstraints().List(kapi.ListOptions{})
	if err != nil {
		if !errors.IsUnauthorized(err) {
			return fmt.Errorf("could not retrieve list of security constraints to verify service account %q: %v", serviceAccount, err)
		}
		return nil
	}

	// get set of sccs applicable to the service account
	userInfo := serviceaccount.UserInfo(ns, serviceAccount, "")
	for _, scc := range sccList.Items {
		if admission.ConstraintAppliesTo(&scc, userInfo) {
			switch {
			case hostNetwork && scc.AllowHostNetwork:
				return nil
			}
		}
	}

	errMsg := "service account %q is not allowed to access the host network on nodes, grant access with oadm policy add-scc-to-user %s -z %s"
	return fmt.Errorf(errMsg, serviceAccount, bootstrappolicy.SecurityContextConstraintsHostNetwork, serviceAccount)
}
