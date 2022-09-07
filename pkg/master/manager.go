package master

import (
	"context"
	"errors"
	"fmt"
	"github.com/go-logr/logr"
	"github.com/medik8s/self-node-remediation/pkg/peers"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"math/rand"
	"net"
	"os/exec"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"time"
)

const (
	initErrorText     = "error initializing master handler"
	etcdContainerName = "etcd"
)

var (
	initError    = errors.New(initErrorText)
	processError = errors.New("an error occurred during master remediation process")
)

//Manager contains logic and info needed to fence and remediate master nodes
type Manager struct {
	nodeName string
	nodeRole peers.Role
	//TODO mshitrit remove if not used
	nodeNameRoleMapping map[string]peers.Role
	isHasInternetAccess bool
	client              client.Client
	log                 logr.Logger
	exec                podCommandExecuter
	etcdPod             corev1.Pod
}

//NewManager inits a new Manager return nil if init fails
func NewManager(nodeName string, myClient client.Client) *Manager {
	return &Manager{
		nodeName:            nodeName,
		nodeNameRoleMapping: map[string]peers.Role{},
		client:              myClient,
		isHasInternetAccess: false,
		log:                 ctrl.Log.WithName("master").WithName("Manager"),
		exec:                &basePodCommandExecuter{ctrl.Log.WithName("master").WithName("PodCommandExecuter")},
	}
}

func (manager *Manager) Start(ctx context.Context) error {
	if err := manager.initializeManager(); err != nil {
		return err
	}
	//TODO mshitrit remove later, only for debug
	manager.log.Info("[DEBUG] current node role is:", "role", manager.nodeRole)
	manager.log.Info("[DEBUG] node name -> role mapping: ", "mapping", manager.nodeNameRoleMapping)
	go func() {
		for {
			_ = manager.isEtcdRunning()
			//_ = manager.isKubeletServiceRunning()
			time.Sleep(time.Second * 5)
		}
	}()

	return nil
}

func (manager *Manager) IsMaster() bool {
	return manager.nodeRole == peers.Master
}

func (manager *Manager) IsMasterHealthy(workerPeerResponse peers.Response, isOtherMastersCanBeReached bool) bool {
	switch workerPeerResponse.Reason {
	//reported unhealthy by worker peers
	case peers.UnHealthyBecauseCRFound:
		return false
	case peers.UnHealthyBecauseNodeIsIsolated:
		return isOtherMastersCanBeReached
	//reported healthy by worker peers
	case peers.HealthyBecauseErrorsThresholdNotReached, peers.HealthyBecauseCRNotFound:
		return true
	//master node has connection to most workers, we assume it's not isolated (or at least that the master node that does not have worker peers quorum will reboot)
	case peers.HealthyBecauseMostPeersCantAccessAPIServer:
		//TODO mshitrit error is ignored
		isHealthy, _ := manager.isDiagnosticsPassed()
		return isHealthy
	case peers.HealthyBecauseNoPeersWereFound:
		if isHealthy, _ := manager.isDiagnosticsPassed(); !isHealthy {
			return false
		}
		return isOtherMastersCanBeReached

	default:
		manager.log.Error(processError, "node is considered unhealthy by worker peers for an unknown reason", "reason", workerPeerResponse.Reason, "node name", manager.nodeName)
		return false
	}

}

func (manager *Manager) isDiagnosticsPassed() (bool, error) {
	//TODO mshitrit implement check external communication, kubelet service etc
	if isLostInternetConnection := manager.isHasInternetAccess && !isHasInternetAccess(); isLostInternetConnection {
		return false, nil
	} else if !manager.isKubeletServiceRunning() {
		return false, nil
	}

	randomBool := rand.Intn(2) == 0
	return randomBool, nil
}

func wrapWithInitError(err error) error {
	return fmt.Errorf(initErrorText+" [%w]", err)
}

func (manager *Manager) initializeManager() error {
	nodesList := &corev1.NodeList{}
	if err := manager.client.List(context.TODO(), nodesList, &client.ListOptions{}); err != nil {
		manager.log.Error(err, "could not retrieve nodes")
		return wrapWithInitError(err)
	}

	for _, node := range nodesList.Items {

		isNodeRoleFound := false
		if _, isWorker := node.Labels[peers.WorkerLabelName]; isWorker {
			manager.nodeNameRoleMapping[node.Name] = peers.Worker
			isNodeRoleFound = true
		} else {
			peers.SetControlPlaneLabelType(&node)
			if _, isMaster := node.Labels[peers.GetUsedControlPlaneLabel()]; isMaster {
				manager.nodeNameRoleMapping[node.Name] = peers.Master
				isNodeRoleFound = true
			}
		}

		if !isNodeRoleFound {
			manager.log.Error(initError, "could not find role for node", "node name", node.Name)
			return initError
		}
	}

	if _, isFound := manager.nodeNameRoleMapping[manager.nodeName]; !isFound {
		manager.log.Error(initError, "could not find role for current node", "node name", manager.nodeName)
		return initError
	} else {
		manager.nodeRole = manager.nodeNameRoleMapping[manager.nodeName]
		manager.isHasInternetAccess = isHasInternetAccess()
		manager.log.Info("[DEBUG] internet connection status is:", "status", manager.isHasInternetAccess)
		return nil
	}

}
func (manager *Manager) fetchEtcdPod() corev1.Pod {
	podsList := &corev1.PodList{}
	labelSelector := metav1.LabelSelector{MatchLabels: map[string]string{"etcd": "true"}}
	selector, _ := metav1.LabelSelectorAsSelector(&labelSelector)
	listOptions := client.ListOptions{
		LabelSelector: selector,
	}
	if err := manager.client.List(context.TODO(), podsList, &listOptions); err != nil {
		manager.log.Info("[DEBUG] etcd pod isn't found, couldn't fetch any pods")
		return manager.etcdPod
	}
	isFound := false
	for _, pod := range podsList.Items {
		if pod.Name == fmt.Sprintf("etcd-%s", manager.nodeName) {
			manager.etcdPod = pod
			manager.log.Info("[DEBUG] etcd pod found")
			isFound = true
			break
		}
	}
	if !isFound {
		manager.log.Info("[DEBUG] etcd pod isn't found", "#pods fetched", len(podsList.Items))
	}

	return manager.etcdPod
}

func isHasInternetAccess() bool {
	con, err := net.DialTimeout("tcp", "google.com:80", 5*time.Second)
	defer func(con net.Conn) {
		_ = con.Close()
	}(con)
	return err == nil
}

func (manager *Manager) isKubeletServiceRunning() bool {
	url := fmt.Sprintf("https://%s:10250/pods", manager.nodeName)
	cmd := exec.Command("curl", "-k", "-X", "GET", url)
	if err := cmd.Run(); err != nil {
		manager.log.Error(err, "kubelet service is down", "node name", manager.nodeName)
		return false
	}
	return true
}

func (manager *Manager) isEtcdRunning() bool {
	etcdPod := manager.fetchEtcdPod()
	stdout, stderr, err := manager.exec.execCmdOnPod([]string{"etcdctl", "version"}, &etcdPod, etcdContainerName)
	manager.log.Info("[DEBUG] isEtcdRunning results", "stdout", stdout, "stderr", stderr, "err", err)
	return true
}
