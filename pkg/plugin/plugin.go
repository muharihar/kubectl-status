//go:generate statik -src templates/
package plugin

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"strconv"
	"strings"
	"text/template"
	"time"
	_ "unsafe"

	"github.com/Masterminds/sprig/v3"
	_ "github.com/bergerx/kubectl-status/pkg/plugin/statik"
	"github.com/dustin/go-humanize"
	"github.com/fatih/color"
	"github.com/pkg/errors"
	sfs "github.com/rakyll/statik/fs"
	"github.com/spf13/cobra"
	resource2 "k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/cli-runtime/pkg/resource"
	"k8s.io/client-go/kubernetes"
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/scheme"
	metricsv "k8s.io/metrics/pkg/client/clientset/versioned"
)

var funcMap = template.FuncMap{
	"green":                 color.GreenString,
	"yellow":                color.YellowString,
	"red":                   color.RedString,
	"cyan":                  color.CyanString,
	"bold":                  color.New(color.Bold).SprintfFunc(),
	"colorAgo":              colorAgo,
	"colorDuration":         colorDuration,
	"colorBool":             colorBool,
	"colorKeyword":          colorKeyword,
	"colorExitCode":         colorExitCode,
	"markRed":               markRed,
	"markYellow":            markYellow,
	"redIf":                 redIf,
	"signalName":            signalName,
	"isPodConditionHealthy": isPodConditionHealthy,
	"quantityToFloat64":     quantityToFloat64,
	"quantityToInt64":       quantityToInt64,
	"percent":               percent,
	"humanizeSI":            humanizeSI,
	"getItemInList":         getItemInList,
}

func humanizeSI(unit string, input float64) string {
	return humanize.SIWithDigits(input, 1, unit)
}

func quantityToFloat64(str string) float64 {
	quantity, _ := resource2.ParseQuantity(str)
	return float64(quantity.MilliValue()) / 1000
}

func quantityToInt64(str string) int64 {
	quantity, _ := resource2.ParseQuantity(str)
	return quantity.Value()
}

func percent(x, y float64) float64 {
	return x / y * 100
}

func colorBool(cond bool, str string) string {
	if cond {
		return color.GreenString(str)
	} else {
		return color.New(color.FgRed, color.Bold).Sprintf(str)
	}
}

func getItemInList(list []interface{}, itemKey, itemValue string) map[string]interface{} {
	var item map[string]interface{}
	for _, untypedItem := range list {
		typedItem := untypedItem.(map[string]interface{})
		if typedItem[itemKey].(string) == itemValue {
			item = typedItem
			break
		}
	}
	return item
}

func isPodConditionHealthy(condition map[string]interface{}) bool {
	switch {
	/*
		From https://github.com/kubernetes/community/blob/master/contributors/devel/sig-architecture/api-conventions.md#typical-status-properties:

		> Condition types should indicate state in the "abnormal-true" polarity. For example, if the condition indicates
		> when a policy is invalid, the "is valid" case is probably the norm, so the condition should be called
		> "Invalid".

		But apparently this is not common among most resources, so we have the list of cases that matches the expected
		behaviour rather than the exceptions.
	*/
	case strings.HasSuffix(fmt.Sprint(condition["type"]), "Pressure"), // Node Pressure conditions
		strings.HasSuffix(fmt.Sprint(condition["type"]), "Unavailable"), // Node NetworkUnavailable condition
		strings.HasSuffix(fmt.Sprint(condition["type"]), "Failure"),     // ReplicaSet ReplicaFailure: condition
		strings.HasPrefix(fmt.Sprint(condition["type"]), "Non"),         // CRD NonStructuralSchema condition
		condition["type"] == "Failed":                                   // Failed Jobs has this condition
		switch condition["status"] {
		case "False":
			return true
		case "True", "Unknown":
			return false
		default:
			// not likely to ever happen, but just in case
			return false
		}
	default:
		switch condition["status"] {
		case "True":
			return true
		case "False", "Unknown":
			return false
		default:
			return false
		}
	}
}

//go:linkname signame runtime.signame
func signame(sig uint32) string

func signalName(signal int64) string {
	return signame(uint32(signal))
}

func redIf(cond bool, str string) string {
	if cond {
		return color.RedString(str)
	}
	return str
}

func markRed(substr, s string) string {
	return strings.ReplaceAll(s, substr, color.RedString(substr))
}

func markYellow(substr, s string) string {
	return strings.ReplaceAll(s, substr, color.YellowString(substr))
}

func colorExitCode(exitCode int) string {
	switch exitCode {
	case 0:
		return strconv.Itoa(exitCode)
	default:
		return color.RedString("%d", exitCode)
	}
}

func colorKeyword(phase string) string {
	/* covers ".status.phase", ".status.state", ".status.reason", pod QOS,
		   for various types, e.g. pod, pv, pvc, svc, ns, etc ...

		Here a generic method is used since this can be used to cover unknown CRDs that follows conventions as well.
		This also helps with maintaining the list
	    E.g.:
	    * acme.cert-manager.io/v1alpha2 Order: .status.state: valid
	    * pvc: .status.phase Bound
	*/
	switch phase {
	case "Running", "Succeeded", "Active", "Available", "Bound", "valid", "Guaranteed", "Completed":
		return color.GreenString(phase)
	case "Pending", "Released", "Burstable":
		return color.YellowString(phase)
	case "Failed", "Unknown", "Terminating", "Evicted", "BestEffort", "OOMKilled", "ContainerCannotRun", "Error":
		return color.New(color.FgRed, color.Bold).Sprintf(phase)
	default:
		return phase
	}
}

func colorAgo(kubeDate string) string {
	t, _ := time.ParseInLocation("2006-01-02T15:04:05Z", kubeDate, time.Local)
	duration := time.Since(t).Round(time.Second)
	return colorDuration(duration)
}

func colorDuration(duration time.Duration) string {
	durationRound := (sprig.GenericFuncMap()["durationRound"]).(func(duration interface{}) string)
	str := durationRound(duration.String())
	if duration < time.Minute*5 {
		return color.RedString(str)
	}
	if duration < time.Hour {
		return color.YellowString(str)
	}
	if duration < time.Hour*24 {
		return color.MagentaString(str)
	}
	return str
}

func RunPlugin(f cmdutil.Factory, cmd *cobra.Command, args []string) error {
	//log := logger.NewLogger()
	//log.Info(strings.Join(args, ","))
	clientSet, err := f.KubernetesClientSet()
	clientConfig := f.ToRawKubeConfigLoader()
	namespace, enforceNamespace, err := clientConfig.Namespace()
	if err != nil {
		return errors.WithMessage(err, "Failed getting namespace")
	}
	filenames := cmdutil.GetFlagStringSlice(cmd, "filename")

	r := f.NewBuilder().
		Unstructured().
		NamespaceParam(namespace).DefaultNamespace().AllNamespaces(cmdutil.GetFlagBool(cmd, "all-namespaces")).
		FilenameParam(enforceNamespace, &resource.FilenameOptions{Filenames: filenames}).
		LabelSelectorParam(cmdutil.GetFlagString(cmd, "selector")).
		ResourceTypeOrNameArgs(true, args...).
		ContinueOnError().
		Flatten().
		Do()

	err = r.Err()
	if err != nil {
		return errors.WithMessage(err, "Failed during querying of resources")
	}

	contents, err := getTemplate()
	if err != nil {
		return err
	}

	var allErrs []error
	infos, err := r.Infos()
	if err != nil {
		allErrs = append(allErrs, err)
	}
	for _, info := range infos {
		var err error
		out := map[string]interface{}{}
		obj := info.Object
		objKind := info.ResourceMapping().GroupVersionKind.Kind
		err = includeObj(obj, out)
		if err != nil {
			allErrs = append(allErrs, err)
			continue
		}
		err = includeEvents(obj, clientSet, out)
		if err != nil {
			allErrs = append(allErrs, err)
			continue
		}
		if objKind == "Node" {
			err = includeNodeMetrics(obj, f, out)
			if err != nil {
				allErrs = append(allErrs, err)
				continue
			}
		}
		if objKind == "Pod" {
			err = includePodMetrics(obj, f, out)
			if err != nil {
				allErrs = append(allErrs, err)
				continue
			}
		}

		tmpl := template.Must(template.
			New("templates.tmpl").
			Funcs(sprig.TxtFuncMap()).
			Funcs(funcMap).
			Parse(contents))
		var kindTemplateName string
		if t := tmpl.Lookup(objKind); t != nil {
			kindTemplateName = objKind
		} else {
			kindTemplateName = "DefaultResource"
		}

		err = tmpl.ExecuteTemplate(os.Stdout, kindTemplateName, out)
		if err != nil {
			allErrs = append(allErrs, err)
			continue
		}
		// Add a newline at the end of every template
		fmt.Println("")
	}
	return utilerrors.NewAggregate(allErrs)
}

func includeObj(obj runtime.Object, out map[string]interface{}) error {
	var data []byte
	data, err := json.Marshal(obj)
	if err != nil {
		return errors.WithMessage(err, "Failed JSON encoding of object")
	}
	err = json.Unmarshal(data, &out)
	if err != nil {
		return errors.WithMessage(err, "Failed parsing JSON of object")
	}
	return nil
}

func includeEvents(obj runtime.Object, clientSet *kubernetes.Clientset, out map[string]interface{}) error {
	objectMeta := obj.(metav1.Object)
	events, err := clientSet.CoreV1().Events(objectMeta.GetNamespace()).Search(scheme.Scheme, obj)
	if err != nil {
		return errors.WithMessage(err, "Failed getting event")
	}
	eventsJson, err := json.Marshal(events)
	if err != nil {
		return errors.WithMessage(err, "Failed JSON encoding of object's Events")
	}
	eventsKey := make(map[string]interface{})
	err = json.Unmarshal(eventsJson, &eventsKey)
	if err != nil {
		return errors.WithMessage(err, "Failed parsing JSON of Events")
	}
	out["events"] = eventsKey
	return nil
}

func includeNodeMetrics(obj runtime.Object, f cmdutil.Factory, out map[string]interface{}) error {
	config, _ := f.ToRESTConfig()
	clientSet, err := metricsv.NewForConfig(config)
	if err != nil {
		return errors.WithMessage(err, "Failed getting metrics clientSet")
	}
	objectMeta := obj.(metav1.Object)
	nodeMetrics, err := clientSet.MetricsV1beta1().
		NodeMetricses().
		Get(objectMeta.GetName(), v1.GetOptions{})
	if err != nil {
		// swallow any errors while getting NodeMetrics
		return nil
	}
	nodeMetricsJson, err := json.Marshal(nodeMetrics)
	if err != nil {
		return errors.WithMessage(err, "Failed JSON encoding of NodeMetrics")
	}
	nodeMetricsKey := make(map[string]interface{})
	err = json.Unmarshal(nodeMetricsJson, &nodeMetricsKey)
	if err != nil {
		return errors.WithMessage(err, "Failed parsing JSON of NodeMetrics")
	}
	out["nodeMetrics"] = nodeMetricsKey
	return nil
}

func includePodMetrics(obj runtime.Object, f cmdutil.Factory, out map[string]interface{}) error {
	config, _ := f.ToRESTConfig()
	clientSet, err := metricsv.NewForConfig(config)
	if err != nil {
		return errors.WithMessage(err, "Failed getting metrics clientSet")
	}
	objectMeta := obj.(metav1.Object)
	podMetrics, err := clientSet.MetricsV1beta1().
		PodMetricses(objectMeta.GetNamespace()).
		Get(objectMeta.GetName(), metav1.GetOptions{})
	if err != nil {
		// swallow any errors while getting PodMetrics
		return nil
	}
	podMetricsJson, err := json.Marshal(podMetrics)
	if err != nil {
		return errors.WithMessage(err, "Failed JSON encoding of PodMetrics")
	}
	podMetricsKey := make(map[string]interface{})
	err = json.Unmarshal(podMetricsJson, &podMetricsKey)
	if err != nil {
		return errors.WithMessage(err, "Failed parsing JSON of PodMetrics")
	}
	out["podMetrics"] = podMetricsKey
	return nil
}

func getTemplate() (string, error) {
	statikFS, err := sfs.New()
	if err != nil {
		return "", errors.WithMessage(err, "Failed initiating statikFS")
	}

	// Access individual files by their paths.
	templatesFile := "/templates.tmpl"
	t, err := statikFS.Open(templatesFile)
	if err != nil {
		return "", errors.WithMessage(err, "Failed opening template from statikFS")
	}
	defer t.Close()

	contents, err := ioutil.ReadAll(t)
	if err != nil {
		return "", errors.WithMessage(err, "Failed reading template from statikFS")
	}
	return string(contents), nil
}
