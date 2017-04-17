package main

import (
	"encoding/json"
	"flag"
	"time"

	log "github.com/Sirupsen/logrus"
	"github.com/onrik/logrus/filename"
	"github.com/turbobytes/kubemr/pkg/job"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/pkg/api"
	"k8s.io/client-go/pkg/api/unversioned"
	"k8s.io/client-go/pkg/apis/extensions/v1beta1"
	"k8s.io/client-go/pkg/runtime/serializer"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

var (
	kubeconfig *string
)

func init() {
	//Set this for testing purposes... in prod this would always be in-cluster
	kubeconfig = flag.String("kubeconfig", "", "path to kubeconfig, if absent then we use rest.InClusterConfig()")
	flag.Parse()
	//log.SetFormatter(&log.JSONFormatter{})
	filenameHook := filename.NewHook()
	log.AddHook(filenameHook)
}

func ensureTprExists(cl *kubernetes.Clientset) {
	tpr, err := cl.ExtensionsV1beta1().ThirdPartyResources().Get("map-reduce-job.turbobytes.com")
	if err == nil {
		log.Info("TPR exists")
		return
	}
	//Create TPR
	tpr = &v1beta1.ThirdPartyResource{
		Description: "Map reduce job specification",
		Versions:    []v1beta1.APIVersion{v1beta1.APIVersion{Name: "v1alpha1"}},
	}
	tpr.Kind = "ThirdPartyResource"
	tpr.APIVersion = "extensions/v1beta1"
	tpr.Name = "map-reduce-job.turbobytes.com"
	//Try to insert TPR
	tpr, err = cl.ExtensionsV1beta1().ThirdPartyResources().Create(tpr)
	if err != nil {
		log.Fatal(err)
	}
	log.Info(tpr)
}

type jobmanager struct {
	cl        *kubernetes.Clientset
	tprclient *rest.RESTClient
	jobclient *job.Client
}

func newjobmanager(config *rest.Config, cl *kubernetes.Clientset) (*jobmanager, error) {
	j := &jobmanager{
		cl: cl,
	}
	groupversion := unversioned.GroupVersion{
		Group:   "turbobytes.com",
		Version: "v1alpha1",
	}
	config.APIPath = "/apis"
	config.GroupVersion = &groupversion
	dynclient, err := dynamic.NewClient(config)
	if err != nil {
		return nil, err
	}
	config.NegotiatedSerializer = serializer.DirectCodecFactory{CodecFactory: api.Codecs}

	tprclient, err := rest.RESTClientFor(config)
	if err != nil {
		return nil, err
	}
	j.tprclient = tprclient
	j.jobclient = job.NewClient(dynclient)
	log.Fatal(j.jobclient.WatchList())
	return j, nil
}

func (j *jobmanager) jobloop() error {
	for {
		err := j.jobloopSingle()
		if err != nil {
			//TODO: Return error if something critical happens
			log.Error(err)
		}
		time.Sleep(time.Second * 15)
	}
}

func (j *jobmanager) jobloopSingle() error {
	jobList, err := j.jobclient.List()
	if err != nil {
		return err
	}

	for _, jb := range jobList.Items {
		err = j.process(jb)
		if err != nil {
			return err
		}
	}
	return nil
}

func (j *jobmanager) process(jb job.MapReduceJob) error {
	switch jb.Status {
	case "":
		return j.checkspec(jb)
	}
	return nil
}

func (j *jobmanager) checkspec(jb job.MapReduceJob) error {
	err := jb.Spec.Validate()
	if err != nil {
		return j.specfail(jb, err)
	}
	return j.patchjob(jb, jb.Spec.PatchSpecPending())
}

func addPatchObj(path, v interface{}) map[string]interface{} {
	return map[string]interface{}{"op": "add", "path": path, "value": v}
}

func (j *jobmanager) specfail(jb job.MapReduceJob, err error) error {
	if jb.Status != job.StatusFail {
		updateobj := make([]map[string]interface{}, 0)
		//updateobj = append(updateobj, map[string]interface{}{"op": "test", "path": "/foo", "value": "bar"}) Note2self: This is how we might be able to get locks
		updateobj = append(updateobj, addPatchObj("/status", job.StatusFail))
		updateobj = append(updateobj, addPatchObj("/err", err.Error()))
		return j.patchjob(jb, updateobj)
	}
	return nil
}

//Since we use patch with only the fields we wanna update,
//it shouldnt cause issues if multiple operators are doing the same thing.
func (j *jobmanager) patchjob(jb job.MapReduceJob, update []map[string]interface{}) error {
	b, err := json.Marshal(update)
	if err != nil {
		return err
	}
	req := j.tprclient.Patch(api.JSONPatchType).Resource("mapreducejobs").Namespace(jb.Namespace).Name(jb.Name).Body(b)
	log.Info(req.URL())
	b, err = req.DoRaw()
	if err != nil {
		log.Info(string(b))
		return err
	}
	return nil
}

func getconfig() (*rest.Config, error) {
	if *kubeconfig == "" {
		return rest.InClusterConfig()
	}
	return clientcmd.BuildConfigFromFlags("", *kubeconfig)
}

func main() {
	log.Info("Operator starting...")
	// creates the in-cluster config
	config, err := getconfig()
	if err != nil {
		log.Fatal(err)
	}
	// creates the clientset
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Fatal(err)
	}
	ensureTprExists(clientset)
	j, err := newjobmanager(config, clientset)
	if err != nil {
		log.Fatal(err)
	}
	err = j.jobloop()
	if err != nil {
		log.Fatal(err)
	}
	//managejob(clientset)
}
