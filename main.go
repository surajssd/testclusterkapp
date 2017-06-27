// Note: the example only works with the code within the same release/branch.
package main

import (
	"flag"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	log "github.com/Sirupsen/logrus"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"net/http"

	"bytes"

	"github.com/pkg/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	v1 "k8s.io/client-go/pkg/api/v1"
)

func homeDir() string {
	if h := os.Getenv("HOME"); h != "" {
		return h
	}
	return os.Getenv("USERPROFILE") // windows
}

func createClient() (*kubernetes.Clientset, error) {
	var kubeconfig *string
	if home := homeDir(); home != "" {
		kubeconfig = flag.String("kubeconfig", filepath.Join(home, ".kube", "config"), "(optional) absolute path to the kubeconfig file")
	} else {
		kubeconfig = flag.String("kubeconfig", "", "absolute path to the kubeconfig file")
	}
	flag.Parse()

	// use the current context in kubeconfig
	config, err := clientcmd.BuildConfigFromFlags("", *kubeconfig)
	if err != nil {
		panic(err.Error())
	}

	// create the clientset
	return kubernetes.NewForConfig(config)
}

func createNS(clientset *kubernetes.Clientset, name string) (*v1.Namespace, error) {
	ns := &v1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
	}
	return clientset.CoreV1().Namespaces().Create(ns)
}

var KappLoc string
var KubectlLoc string

func FindKapp() (string, error) {
	kapp, err := exec.LookPath("kapp")
	if err != nil {
		return "", errors.Wrap(err, "cannot find kapp")
	}
	log.Infof("kapp location: %s", kapp)
	return kapp, nil
}

func FindKubectl() (string, error) {
	kubectl, err := exec.LookPath("kubectl")
	if err != nil {
		return "", errors.Wrap(err, "cannot find kubectl")
	}
	log.Infof("kubectl location: %s", kubectl)
	return kubectl, nil
}

func RunKapp(files []string) ([]byte, error) {
	args := []string{"generate"}
	for _, file := range files {
		args = append(args, "-f")
		args = append(args, os.ExpandEnv(file))
	}
	cmd := exec.Command(KappLoc, args...)

	var out, stdErr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stdErr

	err := cmd.Run()
	if err != nil {
		return nil, fmt.Errorf("error running %q\n%s %s",
			fmt.Sprintf("kapp %s", strings.Join(args, " ")),
			stdErr.String(), err)
	}
	return out.Bytes(), nil
}

func RunKubeCreate(input []byte, namespace string) error {
	// now deploy using cmdline kubectl
	args := []string{"-n", namespace, "create", "-f", "-"}

	kubectl := exec.Command(KubectlLoc, args...)
	// creating pipes needed
	kIn, err := kubectl.StdinPipe()
	if err != nil {
		return errors.Wrap(err, "cannot create the stdin pipe to kubectl")
	}
	kOut, err := kubectl.StdoutPipe()
	if err != nil {
		return errors.Wrap(err, "cannot create the stdout pipe to kubectl")
	}
	kErr, err := kubectl.StderrPipe()
	if err != nil {
		return errors.Wrap(err, "cannot create the stderr pipe to kubectl")
	}

	// start process
	if err := kubectl.Start(); err != nil {
		return errors.Wrap(err, "cannot start running kubectl command")
	}
	if _, err := kIn.Write(input); err != nil {
		return errors.Wrap(err, "cannot write to the stdin of kubectl command")
	}
	if err := kIn.Close(); err != nil {
		return errors.Wrap(err, "cannot close the kubectl stdin")
	}
	kOutput, err := ioutil.ReadAll(kOut)
	if err != nil {
		return errors.Wrap(err, "error reading from the kubectl output")
	}
	kError, err := ioutil.ReadAll(kErr)
	if err != nil {
		return errors.Wrap(err, "error reading from the kubectl error")
	}

	if err := kubectl.Wait(); err != nil {
		return errors.Wrapf(err, "failed while waiting on kubectl to complete, got: %s", string(kError))
	}
	log.Infof("deployed in namespace: %q\n%s", namespace, string(kOutput))
	return nil
}

func mapkeys(m map[string]int) []string {
	var keys []string
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

func PodsStarted(clientset *kubernetes.Clientset, namespace string, podNames []string) error {
	// convert podNames to map
	podUp := make(map[string]int)
	for _, p := range podNames {
		podUp[p] = 0
	}

	for {
		log.Debugf("pods not started yet: %q", strings.Join(mapkeys(podUp), " "))

		pods, err := clientset.CoreV1().Pods(namespace).List(metav1.ListOptions{})
		if err != nil {
			return errors.Wrap(err, "error while listing all pods")
		}
		// iterate on all pods we care about
		for k := range podUp {
			for _, p := range pods.Items {
				if strings.Contains(p.Name, k) && p.Status.Phase == v1.PodRunning {
					log.Infof("Pod %q started!", p.Name)
					delete(podUp, k)
				}
			}
		}
		if len(podUp) == 0 {
			break
		}
		time.Sleep(1 * time.Second)
	}
	return nil
}

func getEndPoints(clientset *kubernetes.Clientset, namespace string, svcs []ServicePort) (map[string]string, error) {
	// find the minikube ip
	node, err := clientset.CoreV1().Nodes().List(metav1.ListOptions{})
	if err != nil {
		return nil, errors.Wrap(err, "error while listing all nodes")
	}
	nodeIP := node.Items[0].Status.Addresses[0].Address
	log.Debugf("node ip address %s", nodeIP)

	// get all running services
	runningSvcs, err := clientset.CoreV1().Services(namespace).List(metav1.ListOptions{})
	if err != nil {
		return nil, errors.Wrap(err, "error while listing all services")
	}

	endpoint := make(map[string]string)
	for _, svc := range svcs {
		for _, s := range runningSvcs.Items {
			if s.Name == svc.Name {
				for _, p := range s.Spec.Ports {
					if p.Port == svc.Port {
						port := p.NodePort
						v := fmt.Sprintf("http://%s:%d", nodeIP, port)
						k := fmt.Sprintf("%s:%d", svc.Name, svc.Port)
						endpoint[k] = v
					}
				}
			}
		}
	}
	log.Debugf("endpoints: %#v", endpoint)
	return endpoint, nil
}

func pingEndPoints(ep map[string]string) error {

	for {
		for e, u := range ep {
			timeout := time.Duration(5 * time.Second)
			client := http.Client{
				Timeout: timeout,
			}
			respose, err := client.Get(u)
			if err != nil {
				log.Debugf("error while making http request %q for service %q, err: %v", u, e, err)
				time.Sleep(1 * time.Second)
				continue
			}
			if respose.Status == "200 OK" {
				log.Debugf("%q is running!", e)
				delete(ep, e)
			} else {
				return fmt.Errorf("for service %q got %q", e, respose.Status)
			}
		}
		if len(ep) == 0 {
			break
		}
	}
	return nil
}

type ServicePort struct {
	Name string
	Port int32
}

type testData struct {
	TestName         string
	Namespace        string
	InputFiles       []string
	PodStarted       []string
	NodePortServices []ServicePort
}

func RunTests(clientset *kubernetes.Clientset) error {
	tests := []testData{
		{
			TestName:  "Normal Wordpress test",
			Namespace: "wordpress",
			InputFiles: []string{
				"$GOPATH/src/github.com/surajssd/kapp/examples/wordpress/db.yaml",
				"$GOPATH/src/github.com/surajssd/kapp/examples/wordpress/web.yaml",
			},
			PodStarted: []string{"web"},
			NodePortServices: []ServicePort{
				{Name: "wordpress", Port: 8080},
			},
		},
		{
			TestName:  "Testing configMap",
			Namespace: "configmap",
			InputFiles: []string{
				"$GOPATH/src/github.com/surajssd/kapp/examples/configmap/db.yaml",
				"$GOPATH/src/github.com/surajssd/kapp/examples/configmap/web.yaml",
			},
			PodStarted: []string{"web"},
			NodePortServices: []ServicePort{
				{Name: "wordpress", Port: 8080},
			},
		},
		{
			TestName:  "Testing customVol",
			Namespace: "customvol",
			InputFiles: []string{
				"$GOPATH/src/github.com/surajssd/kapp/examples/customVol/db.yaml",
				"$GOPATH/src/github.com/surajssd/kapp/examples/customVol/web.yaml",
			},
			PodStarted: []string{"web"},
			NodePortServices: []ServicePort{
				{Name: "wordpress", Port: 8080},
			},
		},
		{
			TestName:  "Testing health",
			Namespace: "health",
			InputFiles: []string{
				"$GOPATH/src/github.com/surajssd/kapp/examples/health/db.yaml",
				"$GOPATH/src/github.com/surajssd/kapp/examples/health/web.yaml",
			},
			PodStarted: []string{"web"},
			NodePortServices: []ServicePort{
				{Name: "wordpress", Port: 8080},
			},
		},
		{
			TestName:  "Testing healthChecks",
			Namespace: "healthchecks",
			InputFiles: []string{
				"$GOPATH/src/github.com/surajssd/kapp/examples/healthchecks/db.yaml",
				"$GOPATH/src/github.com/surajssd/kapp/examples/healthchecks/web.yaml",
			},
			PodStarted: []string{"web"},
			NodePortServices: []ServicePort{
				{Name: "wordpress", Port: 8080},
			},
		},
	}

	for _, test := range tests {
		log.Infoln("Running:", test.TestName)

		// create a namespace
		_, err := createNS(clientset, test.Namespace)
		if err != nil {
			return errors.Wrapf(err, "error creating namespace")
		}
		log.Debugf("namespace %q created", test.Namespace)

		// run kapp
		convertedOutput, err := RunKapp(test.InputFiles)
		if err != nil {
			return errors.Wrapf(err, "error running kapp")
		}
		//log.Debugln(string(convertedOutput))

		// run kubectl create
		if err := RunKubeCreate(convertedOutput, test.Namespace); err != nil {
			return errors.Wrapf(err, "error running kubectl create")
		}

		// see if the pods are running
		if err := PodsStarted(clientset, test.Namespace, test.PodStarted); err != nil {
			return errors.Wrapf(err, "error finding running pods")
		}

		// get endpoints for all services
		endPoints, err := getEndPoints(clientset, test.Namespace, test.NodePortServices)
		if err != nil {
			return errors.Wrapf(err, "error getting nodes")
		}

		if err := pingEndPoints(endPoints); err != nil {
			return errors.Wrapf(err, "error pinging endpoint")
		}
		log.Infoln("Successfully pinged all endpoints!")

		if err := clientset.CoreV1().Namespaces().Delete(test.Namespace, &metav1.DeleteOptions{}); err != nil {
			return errors.Wrapf(err, "error deleting namespace")
		}
		log.Infof("Successfully deleted namespace: %q", test.Namespace)
	}
	return nil
}

func main() {
	log.SetLevel(log.DebugLevel)
	clientset, err := createClient()
	if err != nil {
		log.Fatalln("error getting kube client: ", err)
	}
	KappLoc, err = FindKapp()
	if err != nil {
		log.Fatalln(err)
	}
	KubectlLoc, err = FindKubectl()
	if err != nil {
		log.Fatalln(err)
	}

	if err := RunTests(clientset); err != nil {
		log.Fatalln(err)
	}
}