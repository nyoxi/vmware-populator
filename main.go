package main

//
// TODO: deployment: fix labels
// TODO: deployment: fix image location

// TODO: how to pass credentials to populator?
// TODO: VDDK side car?

// TODO:
//   * progress reporting?
//   * temporary storage size?
//   * try using same mechanism to download VMX
//

import (
	"flag"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/klog/v2"

	populator_machinery "github.com/kubernetes-csi/lib-volume-populator/populator-machinery"
)

const (
	prefix      = "forklift.konveyor.io"
	apiVersion  = "v1alpha1"
	mountPath   = "/mnt"
	devicePath  = "/dev/block"
	waitTimeout = 60 * time.Second
)

func main() {
	var (
		// Populator
		credentials string
		dcPath      string
		disk        string
		insecure    bool
		rawBlock    bool
		vcenter     string

		// Infrastructure
		mode         string
		httpEndpoint string
		metricsPath  string
		masterURL    string
		kubeconfig   string
		imageName    string
		namespace    string
	)
	flag.StringVar(&mode, "mode", "", "Mode to run in (controller, populate)")

	// Controller args
	flag.StringVar(&kubeconfig, "kubeconfig", "", "Path to a kubeconfig. Only required if out-of-cluster.")
	flag.StringVar(&masterURL, "master", "", "The address of the Kubernetes API server. Overrides any value in kubeconfig. Only required if out-of-cluster.")
	flag.StringVar(&imageName, "image-name", "", "Image to use for populating")
	flag.StringVar(&namespace, "namespace", "hello", "Namespace to deploy controller")

	// Populate args (CR)
	flag.StringVar(&credentials, "credentials", "", "Secret with password to VCenter")
	flag.StringVar(&dcPath, "dc-path", "", "Path to the VM -- DC/Cluster")
	flag.StringVar(&disk, "disk", "", "Disk location in format \"[datastore]/path\"")
	flag.BoolVar(&insecure, "insecure", false, "Whether to use insecure SSL connections")
	flag.BoolVar(&rawBlock, "raw-block", false, "Whether to write to a block device or a file")
	flag.StringVar(&vcenter, "vcenter", "", "Hostname/IP of VCenter")

	// Metrics args
	flag.StringVar(&httpEndpoint, "http-endpoint", "", "The TCP network address where the HTTP server for diagnostics, including metrics and leader election health check, will listen (example: `:8080`). The default is empty string, which means the server is disabled.")
	flag.StringVar(&metricsPath, "metrics-path", "/metrics", "The HTTP path where prometheus metrics will be exposed. Default is `/metrics`.")
	flag.Parse()

	switch mode {
	case "controller":
		const (
			groupName = prefix
			kind      = "VmwarePopulator"
			resource  = "vmwarepopulators"
		)
		var (
			gk  = schema.GroupKind{Group: groupName, Kind: kind}
			gvr = schema.GroupVersionResource{Group: groupName, Version: apiVersion, Resource: resource}
		)
		populator_machinery.RunController(masterURL, kubeconfig, imageName, httpEndpoint, metricsPath,
			namespace, prefix, gk, gvr, mountPath, devicePath, getPopulatorPodArgs)
	case "populate":
		populate(rawBlock, vcenter, credentials, dcPath, disk, insecure)
	default:
		klog.Fatalf("Invalid mode: %s", mode)
	}
}

type Vmware struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec VmwareSpec `json:"spec"`
}

type VmwareSpec struct {
	Credentials string `json:"credentials"`
	DcPath      string `json:"dcPath"`
	Disk        string `json:"disk"`
	Insecure    bool   `json:"insecure"`
	Vcenter     string `json:"vcenter"`
}

func getPopulatorPodArgs(rawBlock bool, u *unstructured.Unstructured) ([]string, error) {
	var vmware Vmware
	err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.UnstructuredContent(), &vmware)
	if err != nil {
		return nil, err
	}
	args := []string{"--mode=populate"}
	args = append(args, "--credentials="+vmware.Spec.Credentials)
	args = append(args, "--dc-path="+vmware.Spec.DcPath)
	args = append(args, "--disk="+vmware.Spec.Disk)
	args = append(args, "--insecure="+strconv.FormatBool(vmware.Spec.Insecure))
	args = append(args, "--raw-block="+strconv.FormatBool(rawBlock))
	args = append(args, "--vcenter="+vmware.Spec.Vcenter)
	return args, nil
}

func populate(rawBlock bool, vcenter, credentials, dcPath, disk string, insecure bool) {
	if "" == credentials || "" == dcPath || "" == disk || "" == vcenter {
		klog.Fatalf("Missing required arg")
	}

	// TODO: this should be part of the secret
	const (
		username = "administrator@vsphere.local"
	)
	password := credentials

	_, nbdSocket, pidFile := prepareVCenter(vcenter, dcPath, disk, username, password, insecure)

	var outputPath string
	if rawBlock {
		outputPath = devicePath
	} else {
		outputPath = mountPath + "/disk.img"
	}
	nbdPath := fmt.Sprintf("nbd:unix:%s:exportname=/", nbdSocket)
	const (
		overlayPath = "/tmp/overlay.qcow"
	)

	var err error
	var args []string

	// Wait for pid file
	klog.Info("Waiting for nbdkit to start")
	pidTick := time.NewTicker(time.Second)
	pidTimeout := time.NewTimer(waitTimeout)
	for {
		select {
		case <-pidTick.C:
			_, err = os.Stat(pidFile)
		case <-pidTimeout.C:
			klog.Fatal("Timed out waiting for nbdkit to start!")
		}
		if err == nil {
			break
		}
	}
	pidTick.Stop()
	pidTimeout.Stop()

	// Create overlay
	args = []string{
		"create",
		"-o", "compat=1.1",
		"-b", nbdPath,
		"-F", "raw",
		"-f", "qcow2",
		overlayPath,
	}
	klog.Infof("Creating overlay with : qemu-img %v", args)
	overlayCmd := exec.Command("qemu-img", args...)
	overlayCmd.Stdout = os.Stdout
	overlayCmd.Stderr = os.Stderr
	err = overlayCmd.Run()
	if err != nil {
		klog.Fatal(err)
	}

	// Run virt-sparsify
	// TODO: Don't do this on snashot for warm migration, could potentialy
	// cause disk corruption.
	args = []string{
		"-v", "-x",
		"--in-place",
		overlayPath,
	}

	klog.Infof("Running %v with args: %v", "virt-sparsify", args)
	sparsifyCmd := exec.Command("virt-sparsify", args...)
	sparsifyCmd.Env = append(os.Environ(),
		"LIBGUESTFS_BACKEND=direct",
	)
	sparsifyCmd.Stdout = os.Stdout
	sparsifyCmd.Stderr = os.Stderr
	err = sparsifyCmd.Run()
	if err != nil {
		klog.Fatal(err)
	}

	// Run qemu-img convert to pull the missing content and convert the
	// disk to raw.
	args = []string{
		"convert",
		"-p",
		"-f", "qcow2",
		"-O", "raw",
		overlayPath,
		outputPath,
	}
	klog.Infof("Copying disk to destination with: qemu-img %v", args)
	convertCmd := exec.Command("qemu-img", args...)
	convertCmd.Stdout = os.Stdout
	convertCmd.Stderr = os.Stderr
	err = convertCmd.Run()
	if err != nil {
		klog.Fatal(err)
	}

	// TODO: How to do progress reporting?

	// TODO: Attempt plain disk copy if virt-sparsify fails? Are there
	//       scenarios where this makes sense? E.g. LUKS comes to mind but
	//       that is not supported.

	// TODO: cleanup?

	klog.Info("Finished.")
}

func getVCenterUrl(vcenter, dcPath, disk string) string {
	// TODO: strip snapshot numbers
	pathRE := regexp.MustCompile(`^\[(.*)\] (.*)\.vmdk$`)
	match := pathRE.FindStringSubmatch(disk)
	if match == nil {
		klog.Fatalf("Invalid disk path: %v", disk)
	}
	datastore := match[1]
	path := match[2]

	// It is not properly documented, but JoinPath() takes care of
	// escaping the path components.
	fullPath, err := url.JoinPath("folder", fmt.Sprintf("%s-flat.vmdk", path))
	if err != nil {
		klog.Fatalf("Failed to join path: %v", path)
	}

	vCenterUrl := url.URL{
		Scheme:   "https",
		Host:     vcenter, // hostname:port
		Path:     fullPath,
		RawQuery: fmt.Sprintf("dcPath=%s&dsName=%s", url.QueryEscape(dcPath), url.QueryEscape(datastore)),
	}
	return vCenterUrl.String()
}

func vCenterCookieScript(vcenter, dcPath, disk, username, password string, insecure bool) string {

	const (
		csPath       = "/tmp/cs.sh"
		csConfigPath = "/tmp/curl.config"
	)

	if strings.IndexAny(username, "\":\n") != -1 {
		klog.Fatal("Double quotes, colon, or new line characters not allowd in username!")
	}
	if strings.IndexAny(password, "\"\n") != -1 {
		klog.Fatal("Double quotes or new line characters not allowd in password!")
	}

	configBuilder := strings.Builder{}
	configBuilder.WriteString(fmt.Sprintf("url = \"%s\"\n",
		getVCenterUrl(vcenter, dcPath, disk)))
	configBuilder.WriteString("head\n")
	configBuilder.WriteString("silent\n")
	configBuilder.WriteString(fmt.Sprintf("user = \"%s:%s\"\n", username, password))
	if insecure {
		configBuilder.WriteString("insecure\n")
	}

	err := os.WriteFile(csConfigPath, []byte(configBuilder.String()), 0700)
	if err != nil {
		klog.Fatalf("Failed to write file: %v", err)
	}

	csBuilder := strings.Builder{}

	csBuilder.WriteString("#!/bin/sh -\n")
	csBuilder.WriteString("\n")
	csBuilder.WriteString(fmt.Sprintf("curl --config %s |\n", csConfigPath))
	csBuilder.WriteString(fmt.Sprintf("\tsed -ne '%s'\n",
		`{ s/^Set-Cookie: \([^;]*\);.*/\1/ip }`))

	err = os.WriteFile(csPath, []byte(csBuilder.String()), 0700)
	if err != nil {
		klog.Fatalf("Failed to write file: %v", err)
	}

	return csPath
}

func prepareVCenter(vcenter, dcPath, disk, username, password string, insecure bool) (*exec.Cmd, string, string) {

	const (
		// VMware authentication expires after 30 minutes so we must renew
		// after < 30 minutes.
		cookieScriptRenew = 25 * 60
		pidfile           = "/tmp/nbdkit.pid"
		socket            = "/tmp/nbdkit.sock"
		cor               = "/tmp/nbdkit.cor"
		timeout           = 2000
		threads           = 16
	)

	args := []string{
		"--exit-with-parent",
		"--foreground",
		"--pidfile", pidfile,
		"--unix", socket,
		"--threads", strconv.FormatInt(threads, 10),
		//"--selinux-label", "system_u:object_r:svirt_socket_t:s0",
		"-D", "nbdkit.backend.datapath=0",
		"--verbose",
	}

	// IMPORTANT! Add the COW filter.  It must be furthest away except for the
	// rate filter.
	args = append(args, "--filter", "cow")

	// Caching extents speeds up qemu-img, especially its consecutive
	// block_status requests with req_one=1.
	args = append(args, "--filter", "cacheextents")

	// Retry filter can be used to get around brief interruptions in service.
	// It must be closest to the plugin.
	args = append(args, "--filter", "retry")

	csPath := vCenterCookieScript(vcenter, dcPath, disk, username, password, insecure)
	url := getVCenterUrl(vcenter, dcPath, disk)

	args = append(args,
		"curl",
		fmt.Sprintf("url=%s", url),
		fmt.Sprintf("timeout=%d", timeout),
		fmt.Sprintf("cookie-script=%s", csPath),
		fmt.Sprintf("cookie-script-renew=%d", cookieScriptRenew),
		fmt.Sprintf("sslverify=%t", !insecure),
		fmt.Sprintf("cow-on-read=%s", cor),
	)

	// Run nbdkit
	klog.Infof("Running %v with args: %v", "nbdkit", args)
	cmd := exec.Command("nbdkit", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err := cmd.Start()
	if err != nil {
		klog.Fatalf("Failed to start nbdkit: %v", err)
	}

	return cmd, socket, pidfile
}
