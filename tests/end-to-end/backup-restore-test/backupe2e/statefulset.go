package backupe2e

import (
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/google/uuid"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	. "github.com/smartystreets/goconvey/convey"
	"k8s.io/apimachinery/pkg/api/resource"
)

type statefulSetTest struct {
	statefulSetName, uuid, output string
	coreClient                    *kubernetes.Clientset
}

func NewStatefulSetTest() (*statefulSetTest, error) {

	kubeconfig := os.Getenv("KUBECONFIG")
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return nil, err
	}

	coreClient, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, err
	}
	return &statefulSetTest{
		coreClient:      coreClient,
		statefulSetName: "hello",
		uuid:            uuid.New().String(),
	}, nil
}

func (t *statefulSetTest) TestResources(namespace string) {
	replicas := int32(2)
	err := t.waitForPodDeployment(namespace, replicas, 2*time.Minute)
	So(err, ShouldBeNil)
	host := fmt.Sprintf("http://%s-%v.%s.%s/date", t.statefulSetName, replicas-1, t.statefulSetName, namespace)
	value, err := t.getOutput(host, 2*time.Minute)
	So(err, ShouldBeNil)
	if t.output == "" {
		t.output = value
		So(value, ShouldNotBeEmpty)
	} else {
		So(value, ShouldEqual, t.output)
	}
}

func (t *statefulSetTest) getOutput(host string, waitmax time.Duration) (string, error) {

	tick := time.Tick(2 * time.Second)
	timeout := time.After(waitmax)
	messages := ""

	for {
		select {
		case <-tick:
			resp, err := http.Get(host)
			if err != nil {
				messages += fmt.Sprintf("%+v\n", err)
				break
			}
			if resp.StatusCode == http.StatusOK {
				bodyBytes, err := ioutil.ReadAll(resp.Body)
				if err != nil {
					return "", err
				}
				return string(bodyBytes), nil
			}
			messages += fmt.Sprintf("%+v", err)

		case <-timeout:
			return "", fmt.Errorf("Could not get output:\n %v", messages)
		}
	}

}

func (t *statefulSetTest) CreateResources(namespace string) {
	replicas := int32(2)
	err := t.createService(namespace, replicas)
	So(err, ShouldBeNil)
	err = t.createStatefulSet(namespace, replicas)
	So(err, ShouldBeNil)
}

func (t *statefulSetTest) createStatefulSet(namespace string, replicas int32) error {
	statefulSet := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: t.statefulSetName,
		},
		Spec: appsv1.StatefulSetSpec{
			ServiceName: t.statefulSetName,
			Replicas:    int32Ptr(replicas),
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app": "statefulSet" + t.uuid,
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app": "statefulSet" + t.uuid,
					},
				},
				Spec: corev1.PodSpec{
					InitContainers: []corev1.Container{
						corev1.Container{
							Name:  "busybox",
							Image: "busybox",
							Command: []string{
								"sh", "-c",
								"cat /usr/share/nginx/html/date ; test -e /usr/share/nginx/html/date || date > /usr/share/nginx/html/date",
							},
							VolumeMounts: []corev1.VolumeMount{
								corev1.VolumeMount{
									Name:      "www",
									MountPath: "/usr/share/nginx/html",
								},
							},
						},
					},
					Containers: []corev1.Container{
						corev1.Container{
							Name:  "nginx",
							Image: "nginx:alpine",
							Ports: []corev1.ContainerPort{
								corev1.ContainerPort{
									ContainerPort: 80,
								},
							},
							VolumeMounts: []corev1.VolumeMount{
								corev1.VolumeMount{
									Name:      "www",
									MountPath: "/usr/share/nginx/html",
								},
							},
						},
					},
				},
			},
			VolumeClaimTemplates: []corev1.PersistentVolumeClaim{
				corev1.PersistentVolumeClaim{
					ObjectMeta: metav1.ObjectMeta{
						Name: "www",
					},
					Spec: corev1.PersistentVolumeClaimSpec{
						AccessModes: []corev1.PersistentVolumeAccessMode{
							corev1.ReadWriteOnce,
						},
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceStorage: resource.MustParse("5M"),
							},
						},
					},
				},
			},
		},
	}
	_, err := t.coreClient.AppsV1().StatefulSets(namespace).Create(statefulSet)
	return err
}

func (t *statefulSetTest) createService(namespace string, replicas int32) error {
	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: t.statefulSetName,
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{
				"app": "statefulSet" + t.uuid,
			},
			Ports: []corev1.ServicePort{
				corev1.ServicePort{
					Port:     int32(80),
					Protocol: corev1.ProtocolTCP,
				},
			},
			ClusterIP: "None",
		},
	}
	_, err := t.coreClient.CoreV1().Services(namespace).Create(service)
	return err
}

func (t *statefulSetTest) waitForPodDeployment(namespace string, replicas int32, waitmax time.Duration) error {
	timeout := time.After(waitmax)
	tick := time.Tick(2 * time.Second)

	for {
		select {
		case <-timeout:
			return fmt.Errorf("statefulSet %v could not be created within given time  %v", t.statefulSetName, waitmax)
		case <-tick:
			pods, err := t.coreClient.CoreV1().Pods(namespace).List(metav1.ListOptions{LabelSelector: "app=statefulSet" + t.uuid})
			if err != nil {
				return err
			}
			if len(pods.Items) < int(replicas) {
				break
			}
			if len(pods.Items) > int(replicas) {
				return fmt.Errorf("Deployed %v pod, got %v: %+v", replicas, len(pods.Items), pods)
			}

			stillStarting := false
			errorMessage := ""
			for _, pod := range pods.Items {
				if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == corev1.PodUnknown {
					errorMessage += fmt.Sprintf("Pod in state %v: \n%+v\n", pod.Status.Phase, pod)
				}
				if pod.Status.Phase == corev1.PodPending {
					stillStarting = true
				}
			}
			if errorMessage != "" {
				return fmt.Errorf(errorMessage)
			}
			if stillStarting {
				break
			}
			statefulSet, err := t.coreClient.AppsV1().StatefulSets(namespace).Get(t.statefulSetName, metav1.GetOptions{})
			if err != nil {
				return err
			}

			if statefulSet.Status.ReadyReplicas != int32(len(pods.Items)) {
				break
			}
			return nil
		}
	}
}

func (t statefulSetTest) DeleteResources() {
	// There is not need to be implemented for this test.
}