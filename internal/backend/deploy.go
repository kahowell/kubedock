package backend

import (
	"context"
	"crypto/md5"
	"fmt"
	"io"
	"os"
	"regexp"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/klog"

	"github.com/joyrex2001/kubedock/internal/config"
	"github.com/joyrex2001/kubedock/internal/model/types"
	"github.com/joyrex2001/kubedock/internal/util/exec"
	"github.com/joyrex2001/kubedock/internal/util/podtemplate"
	"github.com/joyrex2001/kubedock/internal/util/portforward"
	"github.com/joyrex2001/kubedock/internal/util/reverseproxy"
	"github.com/joyrex2001/kubedock/internal/util/tar"
)

// DeployState describes the state of a deployment.
type DeployState int

const (
	// DeployPending represents a pending deployment
	DeployPending DeployState = iota
	// DeployFailed represents a failed deployment
	DeployFailed
	// DeployRunning represents a running deployment
	DeployRunning
	// DeployCompleted represents a completed deployment
	DeployCompleted
)

// StartContainer will start given container object in kubernetes and
// waits until it's started, or failed with an error.
func (in *instance) StartContainer(tainr *types.Container) (DeployState, error) {
	state, err := in.startContainer(tainr)
	if state == DeployFailed {
		if klog.V(2) {
			klog.Infof("container %s log output:", tainr.ShortID)
			stop := make(chan struct{}, 1)
			_ = in.GetLogs(tainr, false, 100, stop, os.Stderr)
			close(stop)
		}
		_ = in.cli.CoreV1().Pods(in.namespace).Delete(context.Background(), tainr.GetPodName(), metav1.DeleteOptions{})
	}
	return state, err
}

func (in *instance) startContainer(tainr *types.Container) (DeployState, error) {
	reqlimits, err := tainr.GetResourceRequirements()
	if err != nil {
		return DeployFailed, err
	}

	pulpol, err := tainr.GetImagePullPolicy()
	if err != nil {
		return DeployFailed, err
	}

	pod := &corev1.Pod{}
	if in.podTemplate != "" {
		pod, err = podtemplate.PodFromFile(in.podTemplate)
		if err != nil {
			return DeployFailed, fmt.Errorf("error opening podtemplate: %w", err)
		}
	}

	pod.ObjectMeta.Name = tainr.GetPodName()
	pod.ObjectMeta.Namespace = in.namespace
	pod.ObjectMeta.Labels = in.getLabels(pod.ObjectMeta.Labels, tainr)
	pod.ObjectMeta.Annotations = in.getAnnotations(pod.ObjectMeta.Annotations, tainr)
	pod.Spec.Containers = []corev1.Container{{
		Image:           tainr.Image,
		Name:            "main",
		Command:         tainr.Entrypoint,
		Args:            tainr.Cmd,
		Env:             tainr.GetEnvVar(),
		Ports:           in.getContainerPorts(tainr),
		Resources:       reqlimits,
		ImagePullPolicy: pulpol,
	}}
	pod.Spec.ServiceAccountName = tainr.GetServiceAccountName(pod.Spec.ServiceAccountName)
	pod.Spec.RestartPolicy = corev1.RestartPolicyNever

	seccontext, err := tainr.GetPodSecurityContext(pod.Spec.SecurityContext)
	if err != nil {
		return DeployFailed, err
	}
	pod.Spec.SecurityContext = seccontext

	for _, ps := range in.imagePullSecrets {
		pod.Spec.ImagePullSecrets = append(pod.Spec.ImagePullSecrets, corev1.LocalObjectReference{Name: ps})
	}

	if tainr.HasVolumes() {
		if err := in.addVolumes(tainr, pod); err != nil {
			return DeployFailed, err
		}
	}

	if _, err := in.cli.CoreV1().Pods(in.namespace).Create(context.Background(), pod, metav1.CreateOptions{}); err != nil {
		return DeployFailed, err
	}

	if tainr.HasVolumes() {
		if err := in.copyVolumeFolders(tainr, in.timeOut); err != nil {
			return DeployFailed, err
		}
	}

	state, err := in.waitReadyState(tainr, in.timeOut)
	if err != nil {
		return state, err
	}

	if err := in.MapContainerTCPPorts(tainr); err != nil {
		return DeployFailed, err
	}

	if err := in.createServices(tainr); err != nil {
		return state, err
	}

	return state, nil
}

// CreatePortForwards sets up port-forwards for all available ports that
// are configured in the container.
func (in *instance) CreatePortForwards(tainr *types.Container) {
	if err := in.portForward(tainr, tainr.HostPorts); err != nil {
		klog.Errorf("port-forward failed: %s", err)
	}
	if err := in.portForward(tainr, tainr.MappedPorts); err != nil {
		klog.Errorf("port-forward failed: %s", err)
	}
}

// portForward will create port-forwards for all mapped ports.
func (in *instance) portForward(tainr *types.Container, ports map[int]int) error {
	pod, err := in.cli.CoreV1().Pods(in.namespace).Get(context.Background(), tainr.GetPodName(), metav1.GetOptions{})
	if err != nil {
		return err
	}
	for src, dst := range ports {
		if src < 0 {
			continue
		}
		stop := make(chan struct{}, 1)
		tainr.AddStopChannel(stop)
		go portforward.ToPod(portforward.Request{
			RestConfig: in.cfg,
			Pod:        *pod,
			LocalPort:  src,
			PodPort:    dst,
			StopCh:     stop,
			ReadyCh:    make(chan struct{}, 1),
		})
	}
	return nil
}

// CreateReverseProxies sets up reverse-proxies for all fixed ports that
// are configured in the container.
func (in *instance) CreateReverseProxies(tainr *types.Container) {
	in.reverseProxy(tainr, tainr.HostPorts)
	in.reverseProxy(tainr, tainr.MappedPorts)
}

// reverseProxy will create reverse proxies to given container for
// given ports.
func (in *instance) reverseProxy(tainr *types.Container, ports map[int]int) {
	var wg sync.WaitGroup
	for src, dst := range ports {
		if src < 0 {
			continue
		}
		wg.Add(1)
		go func(src, dst int) {
			defer wg.Done()
			klog.Infof("reverse proxy for %d to %d", src, dst)
			stop := make(chan struct{}, 1)
			tainr.AddStopChannel(stop)
			err := reverseproxy.Proxy(reverseproxy.Request{
				LocalPort:  src,
				RemotePort: dst,
				RemoteIP:   tainr.HostIP,
				StopCh:     stop,
				MaxRetry:   30,
			})
			if err != nil {
				klog.Errorf("error setting up reverse-proxy for %d to %d: %s", src, dst, err)
			}
		}(src, dst)
	}
	wg.Wait()
}

// GetPodIP will return the ip of the given container.
func (in *instance) GetPodIP(tainr *types.Container) (string, error) {
	pod, err := in.cli.CoreV1().Pods(in.namespace).Get(context.Background(), tainr.GetPodName(), metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	return pod.Status.PodIP, nil
}

// createServices will create k8s service objects for each provided
// external name, mapped with provided hostports ports.
func (in *instance) createServices(tainr *types.Container) error {
	for _, svc := range in.getServices(tainr) {
		if _, err := in.cli.CoreV1().Services(in.namespace).Create(context.Background(), &svc, metav1.CreateOptions{}); err != nil {
			return err
		}
	}
	return nil
}

// getServices will return corev1 services objects for the given
// container definition.
func (in *instance) getServices(tainr *types.Container) []corev1.Service {
	svcs := []corev1.Service{}
	ports := tainr.GetServicePorts()
	if len(ports) == 0 {
		// no ports available, can't create a service without ports
		if len(tainr.NetworkAliases) > 0 {
			klog.Infof("ignoring network aliases %v, no ports mapped", tainr.NetworkAliases)
		}
		return svcs
	}
	valid := regexp.MustCompile("^[a-z]([-a-z0-9]*[a-z0-9])?$")
	for _, alias := range tainr.NetworkAliases {
		if ok := valid.MatchString(alias); !ok {
			klog.Infof("ignoring network alias %s, invalid name", alias)
			continue
		}
		svc := corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Namespace:   in.namespace,
				Name:        alias,
				Labels:      in.getLabels(nil, tainr),
				Annotations: in.getAnnotations(nil, tainr),
			},
			Spec: corev1.ServiceSpec{
				Selector: in.getPodMatchLabels(tainr),
				Ports:    []corev1.ServicePort{},
			},
		}
		for src, dst := range ports {
			svc.Spec.Ports = append(svc.Spec.Ports, corev1.ServicePort{
				Name:       fmt.Sprintf("tcp-%d-%d", src, dst),
				Protocol:   corev1.ProtocolTCP,
				Port:       int32(src),
				TargetPort: intstr.IntOrString{IntVal: int32(dst)},
			})
		}
		svcs = append(svcs, svc)
	}
	return svcs
}

// getContainerPorts will return the mapped ports of the container
// as k8s ContainerPorts.
func (in *instance) getContainerPorts(tainr *types.Container) []corev1.ContainerPort {
	res := []corev1.ContainerPort{}
	for _, pp := range tainr.GetContainerTCPPorts() {
		n := fmt.Sprintf("kd-tcp-%d", pp)
		res = append(res, corev1.ContainerPort{ContainerPort: int32(pp), Name: n, Protocol: corev1.ProtocolTCP})
	}
	return res
}

// getLabels will return a map of labels to be added to the container. This
// map contains the labels that link to the container definition, as well
// as additional labels which are used internally by kubedock.
func (in *instance) getLabels(labels map[string]string, tainr *types.Container) map[string]string {
	if labels == nil {
		labels = map[string]string{}
	}
	for k, v := range tainr.Labels {
		kk := in.toKubernetesKey(k)
		kv := in.toKubernetesValue(v)
		if kk == "" && k != "" {
			klog.V(3).Infof("not adding `%s` as a label: incompatible key", k)
			continue
		}
		if kv == "" && v != "" {
			klog.V(3).Infof("not adding `%s` with value `%s` as a label: incompatible value", k, v)
			continue
		}
		labels[kk] = kv
	}
	for k, v := range config.DefaultLabels {
		labels[k] = v
	}
	labels["kubedock.containerid"] = tainr.ShortID
	return labels
}

// getAnnotations will return a map of annotations to be added to the
// container. This map contains the labels as specified in the container
// definition.
func (in *instance) getAnnotations(annotations map[string]string, tainr *types.Container) map[string]string {
	if annotations == nil {
		annotations = map[string]string{}
	}
	for k, v := range tainr.Labels {
		annotations[k] = v
	}
	annotations["kubedock.containername"] = tainr.Name
	return annotations
}

// getPodMatchLabels will return the map of labels that can be used to
// match running pods for this container.
func (in *instance) getPodMatchLabels(tainr *types.Container) map[string]string {
	return map[string]string{
		"kubedock.containerid": tainr.ShortID,
	}
}

// waitReadyState will wait for the deploymemt to be ready.
func (in *instance) waitReadyState(tainr *types.Container, wait int) (DeployState, error) {
	for max := 0; max < wait; max++ {
		status, err := in.GetContainerStatus(tainr)
		if status != DeployPending || err != nil {
			return status, err
		}
		time.Sleep(time.Second)
	}
	return DeployFailed, fmt.Errorf("timeout starting container")
}

// GetContainerStatus will return the state of the deployed container.
func (in *instance) GetContainerStatus(tainr *types.Container) (DeployState, error) {
	pod, err := in.cli.CoreV1().Pods(in.namespace).Get(context.Background(), tainr.GetPodName(), metav1.GetOptions{})
	if err != nil {
		return DeployFailed, err
	}
	for _, status := range pod.Status.ContainerStatuses {
		term := status.State.Terminated
		ters := status.LastTerminationState.Terminated
		if (ters != nil && ters.Reason == "Completed") || (term != nil && term.Reason == "Completed") {
			return DeployCompleted, nil
		}
		if status.RestartCount > 0 {
			return DeployFailed, fmt.Errorf("failed to start container")
		}
		if status.State.Waiting != nil && status.State.Waiting.Reason == "ImagePullBackOff" {
			return DeployFailed, fmt.Errorf("failed to start container; error pulling image")
		}
		if status.State.Running != nil {
			return DeployRunning, nil
		}
	}
	if pod.Status.Phase == corev1.PodFailed {
		return DeployFailed, fmt.Errorf("failed to start container")
	}
	return DeployPending, nil
}

// waitInitContainerRunning will wait for a specific container in the
// deployment to be ready.
func (in *instance) waitInitContainerRunning(tainr *types.Container, name string, wait int) error {
	for max := 0; max < wait; max++ {
		pod, err := in.cli.CoreV1().Pods(in.namespace).Get(context.Background(), tainr.GetPodName(), metav1.GetOptions{})
		if err != nil {
			return err
		}
		if pod.Status.Phase == corev1.PodFailed {
			return fmt.Errorf("failed to start container")
		}
		for _, status := range pod.Status.InitContainerStatuses {
			if status.Name != name {
				continue
			}
			if status.State.Running != nil {
				return nil
			}
		}
		time.Sleep(time.Second)
	}
	return fmt.Errorf("timeout starting container")
}

// addVolumes will add an init-container "setup" and creates volumes and
// volume mounts in both the init container and "main" container in order
// to copy data before the container is started. If files are inclueded,
// rather than folders, it will create a configmap, and mounts the files
// from this created configmap.
func (in *instance) addVolumes(tainr *types.Container, pod *corev1.Pod) error {
	pulpol, err := tainr.GetImagePullPolicy()
	if err != nil {
		return err
	}

	pod.Spec.InitContainers = []corev1.Container{{
		Name:            "setup",
		Image:           in.initImage,
		ImagePullPolicy: pulpol,
		Command:         []string{"sh", "-c", "while [ ! -f /tmp/done ]; do sleep 0.1 ; done"},
	}}

	volumes := []corev1.Volume{}
	mounts := []corev1.VolumeMount{}

	for dst := range tainr.GetVolumeFolders() {
		id := in.toKubernetesName(dst)
		volumes = append(volumes,
			corev1.Volume{Name: id, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}})
		mounts = append(mounts, corev1.VolumeMount{Name: id, MountPath: dst})
	}

	vfiles := tainr.GetVolumeFiles()
	if len(vfiles) > 0 {
		cm, err := in.createConfigMapFromFiles(tainr, vfiles)
		if err != nil {
			return err
		}
		volumes = append(volumes, corev1.Volume{
			Name: "vfiles",
			VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{
					Name: cm.ObjectMeta.Name,
				},
			}},
		})
		for dst, src := range vfiles {
			mounts = append(mounts, corev1.VolumeMount{
				Name:      "vfiles",
				MountPath: dst,
				SubPath:   in.fileID(src),
			})
		}
	}

	pfiles := tainr.GetPreArchiveFiles()
	if len(pfiles) > 0 {
		cm, err := in.createConfigMapFromRaw(tainr, pfiles)
		if err != nil {
			return err
		}
		volumes = append(volumes, corev1.Volume{
			Name: "pfiles",
			VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{
					Name: cm.ObjectMeta.Name,
				},
			}},
		})
		for dst := range pfiles {
			mounts = append(mounts, corev1.VolumeMount{
				Name:      "pfiles",
				MountPath: dst,
				SubPath:   in.fileID(dst),
			})
		}
	}

	pod.Spec.Volumes = volumes
	pod.Spec.Containers[0].VolumeMounts = mounts
	pod.Spec.InitContainers[0].VolumeMounts = mounts

	return nil
}

// createConfigMapFromFiles will create a configmap with given name, and adds
// given files to it. If failed, it will return an error.
func (in *instance) createConfigMapFromFiles(tainr *types.Container, files map[string]string) (*corev1.ConfigMap, error) {
	dat := map[string][]byte{}
	for _, dst := range files {
		d, err := in.readFile(dst)
		if err != nil {
			return nil, err
		}
		klog.V(3).Infof("adding %s to configmap %s", dst, tainr.ShortID)
		dat[in.fileID(dst)] = d
	}
	cm := corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:        tainr.ShortID + "-vf",
			Namespace:   in.namespace,
			Labels:      in.getLabels(nil, tainr),
			Annotations: in.getAnnotations(nil, tainr),
		},
		BinaryData: dat,
	}
	return in.cli.CoreV1().ConfigMaps(in.namespace).Create(context.Background(), &cm, metav1.CreateOptions{})
}

// createConfigMapFromRaw will create a configmap with given name, and adds
// given files to it. If failed, it will return an error.
func (in *instance) createConfigMapFromRaw(tainr *types.Container, files map[string][]byte) (*corev1.ConfigMap, error) {
	dat := map[string][]byte{}
	for src, d := range files {
		klog.V(3).Infof("adding %s to configmap %s", src, tainr.ShortID)
		dat[in.fileID(src)] = d
	}
	cm := corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:        tainr.ShortID + "-pf",
			Namespace:   in.namespace,
			Labels:      in.getLabels(nil, tainr),
			Annotations: in.getAnnotations(nil, tainr),
		},
		BinaryData: dat,
	}
	return in.cli.CoreV1().ConfigMaps(in.namespace).Create(context.Background(), &cm, metav1.CreateOptions{})
}

// copyVolumeFolders will copy the configured volumes of the container to
// the running init container, and signal the init container when finished
// with copying.
func (in *instance) copyVolumeFolders(tainr *types.Container, wait int) error {
	if err := in.waitInitContainerRunning(tainr, "setup", wait); err != nil {
		return err
	}

	pod, err := in.cli.CoreV1().Pods(in.namespace).Get(context.Background(), tainr.GetPodName(), metav1.GetOptions{})
	if err != nil {
		return err
	}

	volumes := tainr.GetVolumeFolders()
	for dst, src := range volumes {
		reader, writer := io.Pipe()
		go func() {
			defer writer.Close()
			if err := tar.PackFolder(src, writer); err != nil {
				klog.Errorf("error during tar: %s", err)
				return
			}
		}()
		if err := exec.RemoteCmd(exec.Request{
			Client:     in.cli,
			RestConfig: in.cfg,
			Pod:        *pod,
			Container:  "setup",
			Cmd:        []string{"tar", "-xf", "-", "-C", dst},
			Stdin:      reader,
		}); err != nil {
			klog.Warningf("error during copy: %s", err)
		}
	}

	return in.signalDone(tainr)
}

// fileID will create an unique k8s compatible id to refer to the given file.
func (in *instance) fileID(file string) string {
	return fmt.Sprintf("%x", md5.Sum([]byte(file)))
}

// signalDone will signal the prepare init container to exit.
func (in *instance) signalDone(tainr *types.Container) error {
	pod, err := in.cli.CoreV1().Pods(in.namespace).Get(context.Background(), tainr.GetPodName(), metav1.GetOptions{})
	if err != nil {
		return err
	}
	return exec.RemoteCmd(exec.Request{
		Client:     in.cli,
		RestConfig: in.cfg,
		Pod:        *pod,
		Container:  "setup",
		Cmd:        []string{"touch", "/tmp/done"},
		Stderr:     os.Stderr,
	})
}
