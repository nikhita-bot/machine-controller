package admission

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"reflect"
	"time"

	"github.com/golang/glog"
	"github.com/mattbaird/jsonpatch"

	admissionv1beta1 "k8s.io/api/admission/v1beta1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
)

type admissionData struct {
	coreClient kubernetes.Interface
}

var jsonPatch = admissionv1beta1.PatchTypeJSONPatch

func New(listenAddress string, coreClient kubernetes.Interface) *http.Server {
	m := http.NewServeMux()
	ad := &admissionData{coreClient: coreClient}
	m.HandleFunc("/machinedeployments", handleFuncFactory(ad.mutateMachineDeployments))
	m.HandleFunc("/machines", handleFuncFactory(ad.mutateMachines))
	m.HandleFunc("/healthz", healthZHandler)

	return &http.Server{
		Addr:    listenAddress,
		Handler: http.TimeoutHandler(m, 25*time.Second, "timeout"),
	}
}

func healthZHandler(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
}

func newJSONPatch(original, current runtime.Object) ([]jsonpatch.JsonPatchOperation, error) {
	originalGVK := original.GetObjectKind().GroupVersionKind()
	currentGVK := current.GetObjectKind().GroupVersionKind()
	if !reflect.DeepEqual(originalGVK, currentGVK) {
		return nil, fmt.Errorf("GroupVersionKind %#v is expected to match %#v", originalGVK, currentGVK)
	}
	ori, err := json.Marshal(original)
	if err != nil {
		return nil, err
	}
	glog.V(6).Infof("jsonpatch: Marshaled original: %s", string(ori))
	cur, err := json.Marshal(current)
	if err != nil {
		return nil, err
	}
	glog.V(6).Infof("jsonpatch: Marshaled target: %s", string(cur))
	return jsonpatch.CreatePatch(ori, cur)
}

func createAdmissionResponse(original, mutated runtime.Object) (*admissionv1beta1.AdmissionResponse, error) {
	response := &admissionv1beta1.AdmissionResponse{}
	response.Allowed = true
	if !apiequality.Semantic.DeepEqual(original, mutated) {
		patchOpts, err := newJSONPatch(original, mutated)
		if err != nil {
			return nil, fmt.Errorf("failed to create json patch: %v", err)
		}

		patchRaw, err := json.Marshal(patchOpts)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal json patch: %v", err)
		}
		glog.V(4).Infof("Produced jsonpatch: %s", string(patchRaw))

		response.Patch = patchRaw
		response.PatchType = &jsonPatch
	}
	return response, nil
}

type mutator func(admissionv1beta1.AdmissionReview) (*admissionv1beta1.AdmissionResponse, error)

func handleFuncFactory(mutate mutator) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {

		// We must always return an AdmissionReview with an AdmissionResponse
		// even on error, hence the admissionExecutor  func, this makes error handling much easier
		admissionResponse, err := admissionExecutor(r, mutate)
		if err != nil {
			admissionResponse = &admissionv1beta1.AdmissionResponse{}
			admissionResponse.Result = &metav1.Status{Message: err.Error()}
		}

		admissionReview := admissionv1beta1.AdmissionReview{}
		admissionReview.Response = admissionResponse

		resp, err := json.Marshal(admissionReview)
		if err != nil {
			glog.Errorf("failed to marshal admissionResponse: %v", err)
			return
		}
		if _, err := w.Write(resp); err != nil {
			glog.Errorf("failed to write admissionResponse: %v", err)
		}
	}
}

func admissionExecutor(r *http.Request, mutate mutator) (*admissionv1beta1.AdmissionResponse, error) {
	var body []byte
	if r.Body == nil {
		return nil, fmt.Errorf("request has no body")
	}
	body, err := ioutil.ReadAll(r.Body)
	if err != nil {
		return nil, fmt.Errorf("error reading data from request body: %v", err)
	}

	// verify the content type is accurate
	if contentType := r.Header.Get("Content-Type"); contentType != "application/json" {
		return nil, fmt.Errorf("header Content-Type was %s, expected application/json", contentType)
	}

	admissionReview := admissionv1beta1.AdmissionReview{}
	if err := json.Unmarshal(body, &admissionReview); err != nil {
		return nil, fmt.Errorf("failed to unmarshal request into admissionReview: %v", err)
	}

	admissionResponse, err := mutate(admissionReview)
	if err != nil {
		return nil, fmt.Errorf("defaulting or validation failed: %v", err)
	}

	return admissionResponse, nil
}
