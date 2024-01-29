/*
Copyright 2017 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package editor

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	goruntime "runtime"
	"strings"

	"dup/pkg/duplicate"
	duputil "dup/pkg/util"

	jsonpatch "github.com/evanphx/json-patch"
	"github.com/spf13/cobra"
	"k8s.io/klog/v2"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/mergepatch"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/cli-runtime/pkg/genericiooptions"
	"k8s.io/cli-runtime/pkg/printers"
	"k8s.io/cli-runtime/pkg/resource"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/cmd/util/editor/crlf"
	"k8s.io/kubectl/pkg/scheme"
	"k8s.io/kubectl/pkg/util"
)

var SupportedSubresources = []string{"status"}
var codec = scheme.Codecs.LegacyCodec(scheme.Scheme.PrioritizedVersionsAllGroups()...)

// EditOptions contains all the options for running edit cli command.
type EditOptions struct {
	PrintFlags *genericclioptions.PrintFlags
	ToPrinter  func(string) (printers.ResourcePrinter, error)

	OutputPatch        bool
	WindowsLineEndings bool

	cmdutil.ValidateOptions
	ValidationDirective string

	OriginalResult *resource.Result

	CmdNamespace    string
	ApplyAnnotation bool
	ChangeCause     string

	managedFields map[types.UID][]metav1.ManagedFieldsEntry

	genericiooptions.IOStreams

	f                   cmdutil.Factory
	editPrinterOptions  *editPrinterOptions
	updatedResultGetter func(data []byte) *resource.Result

	FieldManager string

	Subresource string
}

// NewEditOptions returns an initialized EditOptions instance
func NewEditOptions(ioStreams genericiooptions.IOStreams) *EditOptions {
	return &EditOptions{
		PrintFlags: genericclioptions.NewPrintFlags("edited").WithTypeSetter(scheme.Scheme),

		editPrinterOptions: &editPrinterOptions{
			// create new editor-specific PrintFlags, with all
			// output flags disabled, except json / yaml
			printFlags: (&genericclioptions.PrintFlags{
				JSONYamlPrintFlags: genericclioptions.NewJSONYamlPrintFlags(),
			}).WithDefaultOutput("yaml"),
			ext:       ".yaml",
			addHeader: true,
		},

		WindowsLineEndings: goruntime.GOOS == "windows",

		IOStreams: ioStreams,
	}
}

type editPrinterOptions struct {
	printFlags *genericclioptions.PrintFlags
	ext        string
	addHeader  bool
}

func (e *editPrinterOptions) Complete(fromPrintFlags *genericclioptions.PrintFlags) error {
	if e.printFlags == nil {
		return fmt.Errorf("missing PrintFlags in editor printer options")
	}

	// bind output format from existing printflags
	if fromPrintFlags != nil && len(*fromPrintFlags.OutputFormat) > 0 {
		e.printFlags.OutputFormat = fromPrintFlags.OutputFormat
	}

	// prevent a commented header at the top of the user's
	// default editor if presenting contents as json.
	if *e.printFlags.OutputFormat == "json" {
		e.addHeader = false
		e.ext = ".json"
		return nil
	}

	// we default to yaml if check above is false, as only json or yaml are supported
	e.addHeader = true
	e.ext = ".yaml"
	return nil
}

func (e *editPrinterOptions) PrintObj(obj runtime.Object, out io.Writer) error {
	p, err := e.printFlags.ToPrinter()
	if err != nil {
		return err
	}
	return p.PrintObj(obj, out)
}

// Complete completes all the required options
func (o *EditOptions) Complete(f cmdutil.Factory, args []string, cmd *cobra.Command) error {
	var err error
	var podName string

	// Expectation = "cmd [deploy] <outputPod>
	deploymentName := args[0] // TODO: Fix validation checks
	switch len(args) {
	case 1:
		podName = duputil.GetDefaultPodName(deploymentName)
	case 2:
		podName = args[1]
		if !duputil.IsValidPod(podName) {
			return fmt.Errorf("invalid pod name specified: %s", args[0])
		}
	default:
		return fmt.Errorf("Incorrect amount of args, expected 1 or 2, got : %+v", args)
	}

	o.f = f
	o.editPrinterOptions.Complete(o.PrintFlags)
	cmdNamespace, _, err := f.ToRawKubeConfigLoader().Namespace()
	if err != nil {
		return err
	}
	o.CmdNamespace = cmdNamespace

	o.ValidationDirective, err = cmdutil.GetValidationDirective(cmd)
	if err != nil {
		return err
	}

	clientSet, err := o.f.KubernetesClientSet()
	if err != nil {
		return err
	}

	clonedPod, err := duplicate.ClonePod(clientSet, o.CmdNamespace, deploymentName, podName)
	if err != nil {
		return err
	}

	r, err := o.Build(objBody(clonedPod), o.ValidationDirective)
	if err != nil {
		return err
	}

	o.OriginalResult = r

	o.updatedResultGetter = func(data []byte) *resource.Result {
		// resource builder to read objects from edited data
		return f.NewBuilder().
			Unstructured().
			Stream(bytes.NewReader(data), "edited-file").
			ContinueOnError().
			Flatten().
			Do()
	}

	o.ToPrinter = func(operation string) (printers.ResourcePrinter, error) {
		o.PrintFlags.NamePrintFlags.Operation = operation
		return o.PrintFlags.ToPrinter()
	}

	return nil
}

// Validate checks the EditOptions to see if there is sufficient information to run the command.
func (o *EditOptions) Validate() error {
	return nil
}

// Run performs the execution
func (o *EditOptions) Run() error {
	//	CreateDuplicatePod(context.Background(), ioStreams, clientset, deployment, namespace, podName, edit)
	edit := NewDefaultEditor(editorEnvs())
	// editFn is invoked for each edit session (once with a list for normal edit, once for each individual resource in a edit-on-create invocation)
	editFn := func(obj []*resource.Info) error {
		var (
			results = editResults{}
			edited  = []byte{}
			file    string
			err     error
		)

		containsError := false
		// loop until we succeed or cancel editing
		for {
			// get the object we're going to serialize as input to the editor
			originalObj := obj[0].Object

			// generate the file to edit
			buf := &bytes.Buffer{}
			var w io.Writer = buf
			if o.WindowsLineEndings {
				w = crlf.NewCRLFWriter(w)
			}

			if o.editPrinterOptions.addHeader {
				results.header.writeTo(w)
			}

			if !containsError {
				if err := o.extractManagedFields(originalObj); err != nil {
					return preservedFile(err, results.file, o.ErrOut)
				}
				if err := o.editPrinterOptions.PrintObj(originalObj, w); err != nil {
					return preservedFile(err, results.file, o.ErrOut)
				}
			} else {
				// In case of an error, preserve the edited file.
				// Remove the comments (header) from it since we already
				// have included the latest header in the buffer above.
				buf.Write(cmdutil.ManualStrip(edited))
			}

			// launch the editor
			editedDiff := edited
			edited, file, err = edit.LaunchTempFile(fmt.Sprintf("%s-edit-", filepath.Base(os.Args[0])), o.editPrinterOptions.ext, buf)
			if err != nil {
				return preservedFile(err, results.file, o.ErrOut)
			}

			// If we're retrying the loop because of an error, and no change was made in the file, short-circuit
			if containsError && bytes.Equal(cmdutil.StripComments(editedDiff), cmdutil.StripComments(edited)) {
				return preservedFile(fmt.Errorf("%s", "Edit cancelled, no valid changes were saved."), file, o.ErrOut)
			}
			// cleanup any file from the previous pass
			if len(results.file) > 0 {
				os.Remove(results.file)
			}
			klog.V(4).Infof("User edited:\n%s", string(edited))

			// Apply validation
			schema, err := o.f.Validator(o.ValidationDirective)
			if err != nil {
				return preservedFile(err, file, o.ErrOut)
			}

			err = schema.ValidateBytes(cmdutil.StripComments(edited))
			if err != nil {
				results = editResults{
					file: file,
				}
				containsError = true
				fmt.Fprintln(o.ErrOut, results.addError(apierrors.NewInvalid(corev1.SchemeGroupVersion.WithKind("").GroupKind(),
					"", field.ErrorList{field.Invalid(nil, "The edited file failed validation", fmt.Sprintf("%v", err))}), obj[0]))
				continue
			}

			// Compare content without comments
			// if bytes.Equal(cmdutil.StripComments(original), cmdutil.StripComments(edited)) {
			// 	os.Remove(file)
			// 	fmt.Fprintln(o.ErrOut, "Edit cancelled, no changes made.")
			// 	return nil
			// }

			lines, err := hasLines(bytes.NewBuffer(edited))
			if err != nil {
				return preservedFile(err, file, o.ErrOut)
			}
			if !lines {
				os.Remove(file)
				fmt.Fprintln(o.ErrOut, "Edit cancelled, saved file was empty.")
				return nil
			}

			results = editResults{
				file: file,
			}

			// parse the edited file
			updatedInfos, err := o.updatedResultGetter(edited).Infos()
			if err != nil {
				// syntax error
				containsError = true
				results.header.reasons = append(results.header.reasons, editReason{head: fmt.Sprintf("The edited file had a syntax error: %v", err)})
				continue
			}

			// not a syntax error as it turns out...
			containsError = false
			updatedVisitor := resource.InfoListVisitor(updatedInfos)

			// we need to add back managedFields to both updated and original object
			if err := o.restoreManagedFields(updatedInfos); err != nil {
				return preservedFile(err, file, o.ErrOut)
			}
			if err := o.restoreManagedFields(obj); err != nil {
				return preservedFile(err, file, o.ErrOut)
			}

			// need to make sure the original namespace wasn't changed while editing
			if err := updatedVisitor.Visit(resource.RequireNamespace(o.CmdNamespace)); err != nil {
				return preservedFile(err, file, o.ErrOut)
			}

			// iterate through all items to apply annotations
			if err := o.visitAnnotation(updatedVisitor); err != nil {
				return preservedFile(err, file, o.ErrOut)
			}

			err = o.visitToCreate(updatedVisitor)
			if err != nil {
				return preservedFile(err, results.file, o.ErrOut)
			}

			if len(results.edit) == 0 {
				if results.notfound == 0 {
					os.Remove(file)
				} else {
					fmt.Fprintf(o.Out, "The edits you made on deleted resources have been saved to %q\n", file)
				}
				return nil
			}

			if len(results.header.reasons) > 0 {
				containsError = true
			}
		}
	}
	return o.OriginalResult.Visit(func(info *resource.Info, err error) error {
		return editFn([]*resource.Info{info})
	})
}

func (o *EditOptions) extractManagedFields(obj runtime.Object) error {
	o.managedFields = make(map[types.UID][]metav1.ManagedFieldsEntry)
	if meta.IsListType(obj) {
		err := meta.EachListItem(obj, func(obj runtime.Object) error {
			uid, mf, err := clearManagedFields(obj)
			if err != nil {
				return err
			}
			o.managedFields[uid] = mf
			return nil
		})
		return err
	}
	uid, mf, err := clearManagedFields(obj)
	if err != nil {
		return err
	}
	o.managedFields[uid] = mf
	return nil
}

func clearManagedFields(obj runtime.Object) (types.UID, []metav1.ManagedFieldsEntry, error) {
	metaObjs, err := meta.Accessor(obj)
	if err != nil {
		return "", nil, err
	}
	mf := metaObjs.GetManagedFields()
	metaObjs.SetManagedFields(nil)
	return metaObjs.GetUID(), mf, nil
}

func (o *EditOptions) restoreManagedFields(infos []*resource.Info) error {
	for _, info := range infos {
		metaObjs, err := meta.Accessor(info.Object)
		if err != nil {
			return err
		}
		mf := o.managedFields[metaObjs.GetUID()]
		metaObjs.SetManagedFields(mf)
	}
	return nil
}

func (o *EditOptions) visitToApplyEditPatch(originalInfos []*resource.Info, patchVisitor resource.Visitor) error {
	err := patchVisitor.Visit(func(info *resource.Info, incomingErr error) error {
		editObjUID, err := meta.NewAccessor().UID(info.Object)
		if err != nil {
			return err
		}

		var originalInfo *resource.Info
		for _, i := range originalInfos {
			originalObjUID, err := meta.NewAccessor().UID(i.Object)
			if err != nil {
				return err
			}
			if editObjUID == originalObjUID {
				originalInfo = i
				break
			}
		}
		if originalInfo == nil {
			return fmt.Errorf("no original object found for %#v", info.Object)
		}

		originalJS, err := encodeToJSON(originalInfo.Object.(runtime.Unstructured))
		if err != nil {
			return err
		}

		editedJS, err := encodeToJSON(info.Object.(runtime.Unstructured))
		if err != nil {
			return err
		}

		if reflect.DeepEqual(originalJS, editedJS) {
			printer, err := o.ToPrinter("skipped")
			if err != nil {
				return err
			}
			return printer.PrintObj(info.Object, o.Out)
		}
		err = o.annotationPatch(info)
		if err != nil {
			return err
		}

		printer, err := o.ToPrinter("edited")
		if err != nil {
			return err
		}
		return printer.PrintObj(info.Object, o.Out)
	})
	return err
}

func (o *EditOptions) annotationPatch(update *resource.Info) error {
	patch, _, patchType, err := GetApplyPatch(update.Object.(runtime.Unstructured))
	if err != nil {
		return err
	}
	mapping := update.ResourceMapping()
	client, err := o.f.UnstructuredClientForMapping(mapping)
	if err != nil {
		return err
	}
	helper := resource.NewHelper(client, mapping).
		WithFieldManager(o.FieldManager).
		WithFieldValidation(o.ValidationDirective).
		WithSubresource(o.Subresource)
	_, err = helper.Patch(o.CmdNamespace, update.Name, patchType, patch, nil)
	return err
}

// GetApplyPatch is used to get and apply patches
func GetApplyPatch(obj runtime.Unstructured) ([]byte, []byte, types.PatchType, error) {
	beforeJSON, err := encodeToJSON(obj)
	if err != nil {
		return nil, []byte(""), types.MergePatchType, err
	}
	objCopy := obj.DeepCopyObject()
	accessor := meta.NewAccessor()
	annotations, err := accessor.Annotations(objCopy)
	if err != nil {
		return nil, beforeJSON, types.MergePatchType, err
	}
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[corev1.LastAppliedConfigAnnotation] = string(beforeJSON)
	accessor.SetAnnotations(objCopy, annotations)
	afterJSON, err := encodeToJSON(objCopy.(runtime.Unstructured))
	if err != nil {
		return nil, beforeJSON, types.MergePatchType, err
	}
	patch, err := jsonpatch.CreateMergePatch(beforeJSON, afterJSON)
	return patch, beforeJSON, types.MergePatchType, err
}

func encodeToJSON(obj runtime.Unstructured) ([]byte, error) {
	serialization, err := runtime.Encode(unstructured.UnstructuredJSONScheme, obj)
	if err != nil {
		return nil, err
	}
	js, err := yaml.ToJSON(serialization)
	if err != nil {
		return nil, err
	}
	return js, nil
}

func (o *EditOptions) visitToPatch(originalInfos []*resource.Info, patchVisitor resource.Visitor, results *editResults) error {
	err := patchVisitor.Visit(func(info *resource.Info, incomingErr error) error {
		editObjUID, err := meta.NewAccessor().UID(info.Object)
		if err != nil {
			return err
		}

		var originalInfo *resource.Info
		for _, i := range originalInfos {
			originalObjUID, err := meta.NewAccessor().UID(i.Object)
			if err != nil {
				return err
			}
			if editObjUID == originalObjUID {
				originalInfo = i
				break
			}
		}
		if originalInfo == nil {
			return fmt.Errorf("no original object found for %#v", info.Object)
		}

		originalJS, err := encodeToJSON(originalInfo.Object.(runtime.Unstructured))
		if err != nil {
			return err
		}

		editedJS, err := encodeToJSON(info.Object.(runtime.Unstructured))
		if err != nil {
			return err
		}

		if reflect.DeepEqual(originalJS, editedJS) {
			// no edit, so just skip it.
			printer, err := o.ToPrinter("skipped")
			if err != nil {
				return err
			}
			return printer.PrintObj(info.Object, o.Out)
		}

		preconditions := []mergepatch.PreconditionFunc{
			mergepatch.RequireKeyUnchanged("apiVersion"),
			mergepatch.RequireKeyUnchanged("kind"),
			mergepatch.RequireMetadataKeyUnchanged("name"),
			mergepatch.RequireKeyUnchanged("managedFields"),
		}

		// Create the versioned struct from the type defined in the mapping
		// (which is the API version we'll be submitting the patch to)
		versionedObject, err := scheme.Scheme.New(info.Mapping.GroupVersionKind)
		var patchType types.PatchType
		var patch []byte
		switch {
		case runtime.IsNotRegisteredError(err):
			// fall back to generic JSON merge patch
			patchType = types.MergePatchType
			patch, err = jsonpatch.CreateMergePatch(originalJS, editedJS)
			if err != nil {
				klog.V(4).Infof("Unable to calculate diff, no merge is possible: %v", err)
				return err
			}
			var patchMap map[string]interface{}
			err = json.Unmarshal(patch, &patchMap)
			if err != nil {
				klog.V(4).Infof("Unable to calculate diff, no merge is possible: %v", err)
				return err
			}
			for _, precondition := range preconditions {
				if !precondition(patchMap) {
					klog.V(4).Infof("Unable to calculate diff, no merge is possible: %v", err)
					return fmt.Errorf("%s", "At least one of apiVersion, kind and name was changed")
				}
			}
		case err != nil:
			return err
		default:
			patchType = types.StrategicMergePatchType
			patch, err = strategicpatch.CreateTwoWayMergePatch(originalJS, editedJS, versionedObject, preconditions...)
			if err != nil {
				klog.V(4).Infof("Unable to calculate diff, no merge is possible: %v", err)
				if mergepatch.IsPreconditionFailed(err) {
					return fmt.Errorf("%s", "At least one of apiVersion, kind and name was changed")
				}
				return err
			}
		}

		if o.OutputPatch {
			fmt.Fprintf(o.Out, "Patch: %s\n", string(patch))
		}

		patched, err := resource.NewHelper(info.Client, info.Mapping).
			WithFieldManager(o.FieldManager).
			WithFieldValidation(o.ValidationDirective).
			WithSubresource(o.Subresource).
			Patch(info.Namespace, info.Name, patchType, patch, nil)
		if err != nil {
			fmt.Fprintln(o.ErrOut, results.addError(err, info))
			return nil
		}
		info.Refresh(patched, true)
		printer, err := o.ToPrinter("edited")
		if err != nil {
			return err
		}
		return printer.PrintObj(info.Object, o.Out)
	})
	return err
}

func (o *EditOptions) visitToCreate(createVisitor resource.Visitor) error {
	err := createVisitor.Visit(func(info *resource.Info, incomingErr error) error {
		obj, err := resource.NewHelper(info.Client, info.Mapping).
			WithFieldManager(o.FieldManager).
			WithFieldValidation(o.ValidationDirective).
			Create(info.Namespace, true, info.Object)
		if err != nil {
			return err
		}
		info.Refresh(obj, true)
		printer, err := o.ToPrinter("created")
		if err != nil {
			return err
		}
		return printer.PrintObj(info.Object, o.Out)
	})
	return err
}

func (o *EditOptions) visitAnnotation(annotationVisitor resource.Visitor) error {
	// iterate through all items to apply annotations
	err := annotationVisitor.Visit(func(info *resource.Info, incomingErr error) error {
		// put configuration annotation in "updates"
		if o.ApplyAnnotation {
			if err := util.CreateOrUpdateAnnotation(true, info.Object, scheme.DefaultJSONEncoder()); err != nil {
				return err
			}
		}

		return nil

	})
	return err
}

// editReason preserves a message about the reason this file must be edited again
type editReason struct {
	head  string
	other []string
}

// editHeader includes a list of reasons the edit must be retried
type editHeader struct {
	reasons []editReason
}

// writeTo outputs the current header information into a stream
func (h *editHeader) writeTo(w io.Writer) error {
	fmt.Fprint(w, `# Please edit the object below. Lines beginning with a '#' will be ignored,
# and an empty file will abort the edit. If an error occurs while saving this file will be
# reopened with the relevant failures.
#
`)
	for _, r := range h.reasons {
		if len(r.other) > 0 {
			fmt.Fprintf(w, "# %s:\n", hashOnLineBreak(r.head))
		} else {
			fmt.Fprintf(w, "# %s\n", hashOnLineBreak(r.head))
		}
		for _, o := range r.other {
			fmt.Fprintf(w, "# * %s\n", hashOnLineBreak(o))
		}
		fmt.Fprintln(w, "#")
	}
	return nil
}

// editResults capture the result of an update
type editResults struct {
	header    editHeader
	retryable int
	notfound  int
	edit      []*resource.Info
	file      string
}

func (r *editResults) addError(err error, info *resource.Info) string {
	resourceString := info.Mapping.Resource.Resource
	if len(info.Mapping.Resource.Group) > 0 {
		resourceString = resourceString + "." + info.Mapping.Resource.Group
	}

	switch {
	case apierrors.IsInvalid(err):
		r.edit = append(r.edit, info)
		reason := editReason{
			head: fmt.Sprintf("%s %q was not valid", resourceString, info.Name),
		}
		if err, ok := err.(apierrors.APIStatus); ok {
			if details := err.Status().Details; details != nil {
				for _, cause := range details.Causes {
					reason.other = append(reason.other, fmt.Sprintf("%s: %s", cause.Field, cause.Message))
				}
			}
		}
		r.header.reasons = append(r.header.reasons, reason)
		return fmt.Sprintf("error: %s %q is invalid", resourceString, info.Name)
	case apierrors.IsNotFound(err):
		r.notfound++
		return fmt.Sprintf("error: %s %q could not be found on the server", resourceString, info.Name)
	default:
		r.retryable++
		return fmt.Sprintf("error: %s %q could not be patched: %v", resourceString, info.Name, err)
	}
}

// preservedFile writes out a message about the provided file if it exists to the
// provided output stream when an error happens. Used to notify the user where
// their updates were preserved.
func preservedFile(err error, path string, out io.Writer) error {
	if len(path) > 0 {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			fmt.Fprintf(out, "A copy of your changes has been stored to %q\n", path)
		}
	}
	return err
}

// hasLines returns true if any line in the provided stream is non empty - has non-whitespace
// characters, or the first non-whitespace character is a '#' indicating a comment. Returns
// any errors encountered reading the stream.
func hasLines(r io.Reader) (bool, error) {
	// TODO: if any files we read have > 64KB lines, we'll need to switch to bytes.ReadLine
	// TODO: probably going to be secrets
	s := bufio.NewScanner(r)
	for s.Scan() {
		if line := strings.TrimSpace(s.Text()); len(line) > 0 && line[0] != '#' {
			return true, nil
		}
	}
	if err := s.Err(); err != nil && err != io.EOF {
		return false, err
	}
	return false, nil
}

// hashOnLineBreak returns a string built from the provided string by inserting any necessary '#'
// characters after '\n' characters, indicating a comment.
func hashOnLineBreak(s string) string {
	r := ""
	for i, ch := range s {
		j := i + 1
		if j < len(s) && ch == '\n' && s[j] != '#' {
			r += "\n# "
		} else {
			r += string(ch)
		}
	}
	return r
}

// editorEnvs returns an ordered list of env vars to check for editor preferences.
func editorEnvs() []string {
	return []string{
		"KUBE_EDITOR",
		"EDITOR",
	}
}
func objBody(obj runtime.Object) io.ReadCloser {
	return io.NopCloser(bytes.NewReader([]byte(runtime.EncodeOrDie(codec, obj))))
}

// TODO: Placement
func (o *EditOptions) Build(reader io.Reader, validate string) (*resource.Result, error) {
	// Redundant
	schema, err := o.f.Validator(validate)
	if err != nil {
		return nil, err
	}

	result := o.f.NewBuilder().
		Unstructured().
		Schema(schema).
		Stream(reader, "").
		Do()

	err = result.Err()
	if err != nil {
		return nil, err
	}
	return result, nil
}
