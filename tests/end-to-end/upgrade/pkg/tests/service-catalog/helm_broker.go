package service_catalog

import (
	"time"
	"fmt"

	"github.com/pkg/errors"
	"github.com/kubernetes-incubator/service-catalog/pkg/apis/servicecatalog/v1beta1"
	"github.com/kubernetes-incubator/service-catalog/pkg/client/clientset_generated/clientset"
	"github.com/sirupsen/logrus"
	"k8s.io/client-go/kubernetes"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"github.com/go-redis/redis"

	bu "github.com/kyma-project/kyma/components/service-binding-usage-controller/pkg/client/clientset/versioned"
)

const (
	envTesterName         = "env-tester"
	redisInstanceName     = "redis"
	redisBindingName      = "redis-credentials"
	redisBindingUsageName = "redis-bu"

	// key/values used to store in the Redis DB
	sampleKey = "key-001"
	sampleVal = "val-001"
)

type HelmBrokerUpgradeTest struct {
	ServiceCatalogInterface clientset.Interface
	K8sInterface            kubernetes.Interface
	BUInterface             bu.Interface
}

func NewHelmBrokerTest(k8sCli kubernetes.Interface, scCli clientset.Interface, buCli bu.Interface) *HelmBrokerUpgradeTest {
	return &HelmBrokerUpgradeTest{
		K8sInterface: k8sCli,
		ServiceCatalogInterface: scCli,
		BUInterface: buCli,
	}
}

type helmBrokerFlow struct {
	baseFlow

	scInterface clientset.Interface
	buInterface bu.Interface
}

func (ut *HelmBrokerUpgradeTest) CreateResources(stop <-chan struct{}, log logrus.FieldLogger, namespace string) error {
	return ut.newFlow(stop, log, namespace).CreateResources()
}

func (ut *HelmBrokerUpgradeTest) TestResources(stop <-chan struct{}, log logrus.FieldLogger, namespace string) error {
	return ut.newFlow(stop, log, namespace).TestResources()
}

func (ut *HelmBrokerUpgradeTest) newFlow(stop <-chan struct{}, log logrus.FieldLogger, namespace string) *helmBrokerFlow {
	return &helmBrokerFlow{
		baseFlow: baseFlow{
			log:          log,
			stop:         stop,
			namespace:    namespace,
			k8sInterface: ut.K8sInterface,
			scInterface:  ut.ServiceCatalogInterface,
			buInterface:  ut.BUInterface,
		},

		scInterface: ut.ServiceCatalogInterface,
		buInterface: ut.BUInterface,
	}
}

func (ut *HelmBrokerUpgradeTest) Name() string {
	return "Service Catalog"
}

func (f *helmBrokerFlow) CreateResources() error {
	// iterate over steps
	for _, fn := range []func() error{
		f.createRedisInstance,
		f.deployEnvTester,
		f.waitForEnvTester,
		f.waitForRedisInstance,
		f.createRedisBindingAndWaitForReadiness,
		f.createRedisBindingUsageAndWaitForReadiness,
		f.saveValueInRedis,
	} {
		err := fn()
		if err != nil {
			f.logReport()
			return err
		}
	}
	return nil
}

func (f *helmBrokerFlow) TestResources() error {
	// iterate over steps
	for _, fn := range []func() error{
		f.verifyDeploymentContainsRedisEvns,
		f.verifyKeyInRedisExists,
		f.deleteRedisBindingUsage,
		f.verifyDeploymentDoesNotContainRedisEnvs,
		f.deleteRedisBinding,
		f.deleteRedisInstance,
		f.undeployEnvTester,
		f.verifyRedisInstanceRemoved,
		f.verifyAllPodsRemoved,
	} {
		err := fn()
		if err != nil {
			f.logReport()
			return err
		}
	}
	return nil
}

func (f *helmBrokerFlow) logReport() {
	f.logK8SReport()
	f.logServiceCatalogAndBindingUsageReport()
}

func (f *helmBrokerFlow) deployEnvTester() error {
	return f.createEnvTester(envTesterName, "REDIS_PASSWORD")
}

func (f *helmBrokerFlow) createRedisInstance() error {
	f.log.Infof("Creating Redis service instance")
	_, err := f.scInterface.ServicecatalogV1beta1().ServiceInstances(f.namespace).Create(&v1beta1.ServiceInstance{
		TypeMeta: metav1.TypeMeta{
			Kind:       "ServiceInstance",
			APIVersion: "servicecatalog.k8s.io/v1beta1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: redisInstanceName,
		},
		Spec: v1beta1.ServiceInstanceSpec{
			PlanReference: v1beta1.PlanReference{
				ClusterServiceClassExternalName: "redis",
				ClusterServicePlanExternalName:  "micro",
			},
		},
	})
	return err
}

func (f *helmBrokerFlow) createRedisBindingAndWaitForReadiness() error {
	return f.createBindingAndWaitForReadiness(redisBindingName, redisInstanceName)
}

func (f *helmBrokerFlow) createRedisBindingUsageAndWaitForReadiness() error {
	return f.createBindingUsageAndWaitForReadiness(redisBindingUsageName, redisBindingName, envTesterName)
}

func (f *helmBrokerFlow) saveValueInRedis() error {
	f.log.Info("Waiting for Redis to be available")

	client, err := f.redisClient()
	if err != nil {
		return err
	}

	err = f.wait(30 * time.Second, func() (done bool, err error) {
		if client.Ping().Val() == "PONG" {
			return true, nil
		}
		return false, nil
	})
	if err != nil {
		return err
	}

	f.log.Info("Saving a value in the Redis DB")

	var redisErr error
	// if the redis connection fails, do retry
	err = f.wait(5 * time.Second, func() (done bool, err error) {
		// Zero expiration means the key has no expiration time.
		_, redisErr = client.Set(sampleKey, sampleVal, 0).Result()

		// retry if an error occurred
		if redisErr != nil {
			return false, nil
		}
		return true, nil
	})
	if err != nil {
		return err
	}
	return redisErr
}

func (f *helmBrokerFlow) verifyDeploymentContainsRedisEvns() error {
	f.log.Info("Testing environment variable injection")

	creds, err := f.redisCredentials()
	if err != nil {
		return errors.Wrap(err, "while getting redis credentials")
	}

	return f.waitForEnvInjected(envTesterName, "REDIS_PASSWORD", creds.password)
}

func (f *helmBrokerFlow) verifyKeyInRedisExists() error {
	f.log.Info("Verify the value stored in the Redis DB")

	client, err := f.redisClient()
	if err != nil {
		return err
	}

	var redisErr error
	// if the redis connection fails, do retry
	err = f.wait(5 * time.Second, func() (done bool, err error) {
		// Zero expiration means the key has no expiration time.
		_, redisErr = client.Set(sampleKey, sampleVal, 0).Result()
		val, redisErr := client.Get(sampleKey).Result()

		// retry if an error occurred
		if redisErr != nil {
			return false, nil
		}
		// fail if the value is not as expected
		if val != sampleVal {
			return false, fmt.Errorf("the existing value in redis is '%s' but should be '%s'", val, sampleVal)
		}
		return true, nil
	})
	if err != nil {
		return err
	}
	return redisErr
}

func (f *helmBrokerFlow) redisClient() (*redis.Client, error) {
	creds, err := f.redisCredentials()
	if err != nil {
		return nil, err
	}

	client := redis.NewClient(&redis.Options{
		Addr:     fmt.Sprintf("%s:%s", creds.host, creds.port),
		Password: creds.password,
		DB:       0,  // use default DB
	})
	return client, nil
}

// redisCredentials returns host, port, password
func (f *helmBrokerFlow) redisCredentials() (*credentials, error) {
	secret, err := f.k8sInterface.CoreV1().Secrets(f.namespace).Get(redisBindingName, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	data := secret.Data
	return &credentials{
		host: string(data["HOST"]),
		port: string(data["PORT"]),
		password: string(data["REDIS_PASSWORD"]),
	}, nil
}

type credentials struct {
	host     string
	port     string
	password string
}

func (f *helmBrokerFlow) deleteRedisBindingUsage() error {
	return f.deleteBindingUsage(redisBindingUsageName)
}

func (f *helmBrokerFlow) verifyDeploymentDoesNotContainRedisEnvs() error {
	f.log.Info("Verify deployment does not contain redis environments")
	return f.waitForEnvNotInjected(envTesterName, "REDIS_PASSWORD")
}

func (f *helmBrokerFlow) deleteRedisBinding() error {
	return f.deleteServiceBinding(redisBindingName)
}

func (f *helmBrokerFlow) deleteRedisInstance() error {
	return f.deleteServiceInstance(redisInstanceName)
}

func (f *helmBrokerFlow) verifyRedisInstanceRemoved() error {
	return f.waitForInstanceRemoved(redisInstanceName)
}

func (f *helmBrokerFlow) waitForRedisInstance() error {
	f.log.Infof("Waiting for Redis instance to be ready")
	return f.waitForInstance(redisInstanceName)
}

func (f *helmBrokerFlow) waitForEnvTester() error {
	f.log.Info("Waiting for environment variable tester to be ready")
	return f.waitForDeployment(envTesterName)
}

func (f *helmBrokerFlow) undeployEnvTester() error {
	f.log.Info("Removing environment variable tester")
	return f.deleteDeployment(envTesterName)
}

func (f *helmBrokerFlow) verifyAllPodsRemoved() error {
	f.log.Info("Waiting for all Pods to be removed")
	return f.wait(time.Minute, func() (bool, error) {
		l, err := f.k8sInterface.CoreV1().Pods(f.namespace).List(metav1.ListOptions{})

		if err != nil {
			return false, err
		}
		if len(l.Items) == 0 {
			return true, nil
		}
		return false, nil
	})
}