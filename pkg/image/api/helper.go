package api

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"k8s.io/kubernetes/pkg/api/errors"
	"k8s.io/kubernetes/pkg/api/unversioned"
	"k8s.io/kubernetes/pkg/util/sets"

	"github.com/blang/semver"
	"github.com/docker/distribution/digest"
	"github.com/docker/distribution/manifest/schema1"
	"github.com/golang/glog"
)

const (
	// DockerDefaultNamespace is the value for namespace when a single segment name is provided.
	DockerDefaultNamespace = "library"
	// DockerDefaultRegistry is the value for the registry when none was provided.
	DockerDefaultRegistry = "docker.io"
	// DockerDefaultV1Registry is the host name of the default v1 registry
	DockerDefaultV1Registry = "index." + DockerDefaultRegistry
	// DockerDefaultV2Registry is the host name of the default v2 registry
	DockerDefaultV2Registry = "registry-1." + DockerDefaultRegistry

	// containerImageEntrypointAnnotationFormatKey is a format used to identify the entrypoint of a particular
	// container in a pod template. It is a JSON array of strings.
	containerImageEntrypointAnnotationFormatKey = "openshift.io/container.%s.image.entrypoint"
)

// TODO remove (base, tag, id)
func parseRepositoryTag(repos string) (string, string, string) {
	n := strings.Index(repos, "@")
	if n >= 0 {
		parts := strings.Split(repos, "@")
		return parts[0], "", parts[1]
	}
	n = strings.LastIndex(repos, ":")
	if n < 0 {
		return repos, "", ""
	}
	if tag := repos[n+1:]; !strings.Contains(tag, "/") {
		return repos[:n], tag, ""
	}
	return repos, "", ""
}

func isRegistryName(str string) bool {
	switch {
	case strings.Contains(str, ":"),
		strings.Contains(str, "."),
		str == "localhost":
		return true
	}
	return false
}

// IsRegistryDockerHub returns true if the given registry name belongs to
// Docker hub.
func IsRegistryDockerHub(registry string) bool {
	switch registry {
	case DockerDefaultRegistry, DockerDefaultV1Registry, DockerDefaultV2Registry:
		return true
	default:
		return false
	}
}

// ParseDockerImageReference parses a Docker pull spec string into a
// DockerImageReference.
func ParseDockerImageReference(spec string) (DockerImageReference, error) {
	var ref DockerImageReference
	// TODO replace with docker version once docker/docker PR11109 is merged upstream
	stream, tag, id := parseRepositoryTag(spec)

	repoParts := strings.Split(stream, "/")
	switch len(repoParts) {
	case 2:
		if isRegistryName(repoParts[0]) {
			// registry/name
			ref.Registry = repoParts[0]
			if IsRegistryDockerHub(ref.Registry) {
				ref.Namespace = DockerDefaultNamespace
			}
			if len(repoParts[1]) == 0 {
				return ref, fmt.Errorf("the docker pull spec %q must be two or three segments separated by slashes", spec)
			}
			ref.Name = repoParts[1]
			ref.Tag = tag
			ref.ID = id
			break
		}
		// namespace/name
		ref.Namespace = repoParts[0]
		if len(repoParts[1]) == 0 {
			return ref, fmt.Errorf("the docker pull spec %q must be two or three segments separated by slashes", spec)
		}
		ref.Name = repoParts[1]
		ref.Tag = tag
		ref.ID = id
		break
	case 3:
		// registry/namespace/name
		ref.Registry = repoParts[0]
		ref.Namespace = repoParts[1]
		if len(repoParts[2]) == 0 {
			return ref, fmt.Errorf("the docker pull spec %q must be two or three segments separated by slashes", spec)
		}
		ref.Name = repoParts[2]
		ref.Tag = tag
		ref.ID = id
		break
	case 1:
		// name
		if len(repoParts[0]) == 0 {
			return ref, fmt.Errorf("the docker pull spec %q must be two or three segments separated by slashes", spec)
		}
		ref.Name = repoParts[0]
		ref.Tag = tag
		ref.ID = id
		break
	default:
		return ref, fmt.Errorf("the docker pull spec %q must be two or three segments separated by slashes", spec)
	}

	return ref, nil
}

// Equal returns true if the other DockerImageReference is equivalent to the
// reference r. The comparison applies defaults to the Docker image reference,
// so that e.g., "foobar" equals "docker.io/library/foobar:latest".
func (r DockerImageReference) Equal(other DockerImageReference) bool {
	defaultedRef := r.DockerClientDefaults()
	otherDefaultedRef := other.DockerClientDefaults()
	return defaultedRef == otherDefaultedRef
}

// DockerClientDefaults sets the default values used by the Docker client.
func (r DockerImageReference) DockerClientDefaults() DockerImageReference {
	if len(r.Registry) == 0 {
		r.Registry = DockerDefaultRegistry
	}
	if len(r.Namespace) == 0 && IsRegistryDockerHub(r.Registry) {
		r.Namespace = DockerDefaultNamespace
	}
	if len(r.Tag) == 0 {
		r.Tag = DefaultImageTag
	}
	return r
}

// Minimal reduces a DockerImageReference to its minimalist form.
func (r DockerImageReference) Minimal() DockerImageReference {
	if r.Tag == DefaultImageTag {
		r.Tag = ""
	}
	return r
}

// AsRepository returns the reference without tags or IDs.
func (r DockerImageReference) AsRepository() DockerImageReference {
	r.Tag = ""
	r.ID = ""
	return r
}

// RepositoryName returns the registry relative name
func (r DockerImageReference) RepositoryName() string {
	r.Tag = ""
	r.ID = ""
	r.Registry = ""
	return r.Exact()
}

// RepositoryName returns the registry relative name
func (r DockerImageReference) RegistryURL() *url.URL {
	return &url.URL{
		Scheme: "https",
		Host:   r.AsV2().Registry,
	}
}

// DaemonMinimal clears defaults that Docker assumes.
func (r DockerImageReference) DaemonMinimal() DockerImageReference {
	switch r.Registry {
	case DockerDefaultV1Registry, DockerDefaultV2Registry:
		r.Registry = DockerDefaultRegistry
	}
	if IsRegistryDockerHub(r.Registry) && r.Namespace == DockerDefaultNamespace {
		r.Namespace = ""
	}
	return r.Minimal()
}

func (r DockerImageReference) AsV2() DockerImageReference {
	switch r.Registry {
	case DockerDefaultV1Registry, DockerDefaultRegistry:
		r.Registry = DockerDefaultV2Registry
	}
	return r
}

// MostSpecific returns the most specific image reference that can be constructed from the
// current ref, preferring an ID over a Tag. Allows client code dealing with both tags and IDs
// to get the most specific reference easily.
func (r DockerImageReference) MostSpecific() DockerImageReference {
	if len(r.ID) == 0 {
		return r
	}
	if _, err := digest.ParseDigest(r.ID); err == nil {
		r.Tag = ""
		return r
	}
	if len(r.Tag) == 0 {
		r.Tag, r.ID = r.ID, ""
		return r
	}
	return r
}

// NameString returns the name of the reference with its tag or ID.
func (r DockerImageReference) NameString() string {
	switch {
	case len(r.Name) == 0:
		return ""
	case len(r.Tag) > 0:
		return r.Name + ":" + r.Tag
	case len(r.ID) > 0:
		var ref string
		if _, err := digest.ParseDigest(r.ID); err == nil {
			// if it parses as a digest, it's v2 pull by id
			ref = "@" + r.ID
		} else {
			// if it doesn't parse as a digest, it's presumably a v1 registry by-id tag
			ref = ":" + r.ID
		}
		return r.Name + ref
	default:
		return r.Name
	}
}

// Exact returns a string representation of the set fields on the DockerImageReference
func (r DockerImageReference) Exact() string {
	name := r.NameString()
	if len(name) == 0 {
		return name
	}
	s := r.Registry
	if len(s) > 0 {
		s += "/"
	}

	if len(r.Namespace) != 0 {
		s += r.Namespace + "/"
	}
	return s + name
}

// String converts a DockerImageReference to a Docker pull spec (which implies a default namespace
// according to V1 Docker registry rules). Use Exact() if you want no defaulting.
func (r DockerImageReference) String() string {
	if len(r.Namespace) == 0 && IsRegistryDockerHub(r.Registry) {
		r.Namespace = DockerDefaultNamespace
	}
	return r.Exact()
}

// SplitImageStreamTag turns the name of an ImageStreamTag into Name and Tag.
// It returns false if the tag was not properly specified in the name.
func SplitImageStreamTag(nameAndTag string) (name string, tag string, ok bool) {
	parts := strings.SplitN(nameAndTag, ":", 2)
	name = parts[0]
	if len(parts) > 1 {
		tag = parts[1]
	}
	if len(tag) == 0 {
		tag = DefaultImageTag
	}
	return name, tag, len(parts) == 2
}

// JoinImageStreamTag turns a name and tag into the name of an ImageStreamTag
func JoinImageStreamTag(name, tag string) string {
	if len(tag) == 0 {
		tag = DefaultImageTag
	}
	return fmt.Sprintf("%s:%s", name, tag)
}

// NormalizeImageStreamTag normalizes an image stream tag by defaulting to 'latest'
// if no tag has been specified.
func NormalizeImageStreamTag(name string) string {
	if !strings.Contains(name, ":") {
		// Default to latest
		return JoinImageStreamTag(name, DefaultImageTag)
	}
	return name
}

// ManifestMatchesImage returns true if the provided manifest matches the name of the image.
func ManifestMatchesImage(image *Image, newManifest []byte) (bool, error) {
	dgst, err := digest.ParseDigest(image.Name)
	if err != nil {
		return false, err
	}
	v, err := digest.NewDigestVerifier(dgst)
	if err != nil {
		return false, err
	}
	sm := schema1.SignedManifest{Raw: newManifest}
	raw, err := sm.Payload()
	if err != nil {
		return false, err
	}
	if _, err := v.Write(raw); err != nil {
		return false, err
	}
	return v.Verified(), nil
}

// ImageWithMetadata returns a copy of image with the DockerImageMetadata filled in
// from the raw DockerImageManifest data stored in the image.
func ImageWithMetadata(image *Image) error {
	if len(image.DockerImageManifest) == 0 {
		return nil
	}

	manifestData := image.DockerImageManifest

	manifest := DockerImageManifest{}
	if err := json.Unmarshal([]byte(manifestData), &manifest); err != nil {
		return err
	}

	switch manifest.SchemaVersion {
	case 0:
		// legacy config object
	case 1:
		if len(manifest.History) == 0 {
			// should never have an empty history, but just in case...
			return nil
		}

		v1Metadata := DockerV1CompatibilityImage{}
		if err := json.Unmarshal([]byte(manifest.History[0].DockerV1Compatibility), &v1Metadata); err != nil {
			return err
		}

		image.DockerImageLayers = make([]ImageLayer, len(manifest.FSLayers))
		for i, layer := range manifest.FSLayers {
			image.DockerImageLayers[i].Name = layer.DockerBlobSum
		}
		if len(manifest.History) == len(image.DockerImageLayers) {
			image.DockerImageLayers[0].Size = v1Metadata.Size
			var size = DockerV1CompatibilityImageSize{}
			for i, obj := range manifest.History[1:] {
				size.Size = 0
				if err := json.Unmarshal([]byte(obj.DockerV1Compatibility), &size); err != nil {
					continue
				}
				image.DockerImageLayers[i+1].Size = size.Size
			}
		} else {
			glog.V(4).Infof("Imported image has mismatched layer count and history count, not updating image metadata: %s", image.Name)
		}
		// reverse order of the layers for v1 (lowest = 0, highest = i)
		for i, j := 0, len(image.DockerImageLayers)-1; i < j; i, j = i+1, j-1 {
			image.DockerImageLayers[i], image.DockerImageLayers[j] = image.DockerImageLayers[j], image.DockerImageLayers[i]
		}

		image.DockerImageMetadata.ID = v1Metadata.ID
		image.DockerImageMetadata.Parent = v1Metadata.Parent
		image.DockerImageMetadata.Comment = v1Metadata.Comment
		image.DockerImageMetadata.Created = v1Metadata.Created
		image.DockerImageMetadata.Container = v1Metadata.Container
		image.DockerImageMetadata.ContainerConfig = v1Metadata.ContainerConfig
		image.DockerImageMetadata.DockerVersion = v1Metadata.DockerVersion
		image.DockerImageMetadata.Author = v1Metadata.Author
		image.DockerImageMetadata.Config = v1Metadata.Config
		image.DockerImageMetadata.Architecture = v1Metadata.Architecture
		if len(image.DockerImageLayers) > 0 {
			size := int64(0)
			for _, layer := range image.DockerImageLayers {
				size += layer.Size
			}
			image.DockerImageMetadata.Size = size
		} else {
			image.DockerImageMetadata.Size = v1Metadata.Size
		}
	case 2:
		// TODO: need to prepare for this
		return fmt.Errorf("unrecognized Docker image manifest schema %d for %q (%s)", manifest.SchemaVersion, image.Name, image.DockerImageReference)
	default:
		return fmt.Errorf("unrecognized Docker image manifest schema %d for %q (%s)", manifest.SchemaVersion, image.Name, image.DockerImageReference)
	}

	return nil
}

// DockerImageReferenceForStream returns a DockerImageReference that represents
// the ImageStream or false, if no valid reference exists.
func DockerImageReferenceForStream(stream *ImageStream) (DockerImageReference, error) {
	spec := stream.Status.DockerImageRepository
	if len(spec) == 0 {
		spec = stream.Spec.DockerImageRepository
	}
	if len(spec) == 0 {
		return DockerImageReference{}, fmt.Errorf("no possible pull spec for %s/%s", stream.Namespace, stream.Name)
	}
	return ParseDockerImageReference(spec)
}

// FollowTagReference walks through the defined tags on a stream, following any referential tags in the stream.
// Will return ok if the tag is valid, multiple if the tag had at least reference, and ref and finalTag will be the last
// tag seen. If a circular loop is found ok will be false.
func FollowTagReference(stream *ImageStream, tag string) (finalTag string, ref *TagReference, ok bool, multiple bool) {
	seen := sets.NewString()
	for {
		if seen.Has(tag) {
			// circular reference
			return tag, nil, false, multiple
		}
		seen.Insert(tag)

		tagRef, ok := stream.Spec.Tags[tag]
		if !ok {
			// no tag at the end of the rainbow
			return tag, nil, false, multiple
		}
		if tagRef.From == nil || tagRef.From.Kind != "ImageStreamTag" || strings.Contains(tagRef.From.Name, ":") {
			// terminating tag
			return tag, &tagRef, true, multiple
		}

		// follow the referenec
		tag = tagRef.From.Name
		multiple = true
	}
}

// LatestTaggedImage returns the most recent TagEvent for the specified image
// repository and tag. Will resolve lookups for the empty tag. Returns nil
// if tag isn't present in stream.status.tags.
func LatestTaggedImage(stream *ImageStream, tag string) *TagEvent {
	if len(tag) == 0 {
		tag = DefaultImageTag
	}
	// find the most recent tag event with an image reference
	if stream.Status.Tags != nil {
		if history, ok := stream.Status.Tags[tag]; ok {
			if len(history.Items) == 0 {
				return nil
			}
			return &history.Items[0]
		}
	}

	return nil
}

// DifferentTagEvent returns true if the supplied tag event matches the current stream tag event.
// Generation is not compared.
func DifferentTagEvent(stream *ImageStream, tag string, next TagEvent) bool {
	tags, ok := stream.Status.Tags[tag]
	if !ok || len(tags.Items) == 0 {
		return true
	}
	previous := &tags.Items[0]
	sameRef := previous.DockerImageReference == next.DockerImageReference
	sameImage := previous.Image == next.Image
	return !(sameRef && sameImage)
}

// AddTagEventToImageStream attempts to update the given image stream with a tag event. It will
// collapse duplicate entries - returning true if a change was made or false if no change
// occurred. Any successful tag resets the status field.
func AddTagEventToImageStream(stream *ImageStream, tag string, next TagEvent) bool {
	if stream.Status.Tags == nil {
		stream.Status.Tags = make(map[string]TagEventList)
	}

	tags, ok := stream.Status.Tags[tag]
	if !ok || len(tags.Items) == 0 {
		stream.Status.Tags[tag] = TagEventList{Items: []TagEvent{next}}
		return true
	}

	previous := &tags.Items[0]

	sameRef := previous.DockerImageReference == next.DockerImageReference
	sameImage := previous.Image == next.Image
	sameGen := previous.Generation == next.Generation

	switch {
	// shouldn't change the tag
	case sameRef && sameImage && sameGen:
		return false

	case sameImage && sameRef:
		// collapse the tag
	case sameRef:
		previous.Image = next.Image
	case sameImage:
		previous.DockerImageReference = next.DockerImageReference
	default:
		// shouldn't collapse the tag
		tags.Conditions = nil
		tags.Items = append([]TagEvent{next}, tags.Items...)
		stream.Status.Tags[tag] = tags
		return true
	}
	previous.Generation = next.Generation
	tags.Conditions = nil
	stream.Status.Tags[tag] = tags
	return true
}

// UpdateChangedTrackingTags identifies any tags in the status that have changed and
// ensures any referenced tracking tags are also updated. It returns the number of
// updates applied.
func UpdateChangedTrackingTags(new, old *ImageStream) int {
	changes := 0
	for newTag, newImages := range new.Status.Tags {
		if len(newImages.Items) == 0 {
			continue
		}
		if old != nil {
			oldImages := old.Status.Tags[newTag]
			changed, deleted := tagsChanged(newImages.Items, oldImages.Items)
			if !changed || deleted {
				continue
			}
		}
		changes += UpdateTrackingTags(new, newTag, newImages.Items[0])
	}
	return changes
}

// tagsChanged returns true if the two lists differ, and if the newer list is empty
// then deleted is returned true as well.
func tagsChanged(new, old []TagEvent) (changed bool, deleted bool) {
	switch {
	case len(old) == 0 && len(new) == 0:
		return false, false
	case len(new) == 0:
		return true, true
	case len(old) == 0:
		return true, false
	default:
		return new[0] == old[0], false
	}
}

// UpdateTrackingTags sets updatedImage as the most recent TagEvent for all tags
// in stream.spec.tags that have from.kind = "ImageStreamTag" and the tag in from.name
// = updatedTag. from.name may be either <tag> or <stream name>:<tag>. For now, only
// references to tags in the current stream are supported.
//
// For example, if stream.spec.tags[latest].from.name = 2.0, whenever an image is pushed
// to this stream with the tag 2.0, status.tags[latest].items[0] will also be updated
// to point at the same image that was just pushed for 2.0.
//
// Returns the number of tags changed.
func UpdateTrackingTags(stream *ImageStream, updatedTag string, updatedImage TagEvent) int {
	updated := 0
	glog.V(5).Infof("UpdateTrackingTags: stream=%s/%s, updatedTag=%s, updatedImage.dockerImageReference=%s, updatedImage.image=%s", stream.Namespace, stream.Name, updatedTag, updatedImage.DockerImageReference, updatedImage.Image)
	for specTag, tagRef := range stream.Spec.Tags {
		glog.V(5).Infof("Examining spec tag %q, tagRef=%#v", specTag, tagRef)

		// no from
		if tagRef.From == nil {
			glog.V(5).Infof("tagRef.From is nil, skipping")
			continue
		}

		// wrong kind
		if tagRef.From.Kind != "ImageStreamTag" {
			glog.V(5).Infof("tagRef.Kind %q isn't ImageStreamTag, skipping", tagRef.From.Kind)
			continue
		}

		tagRefNamespace := tagRef.From.Namespace
		if len(tagRefNamespace) == 0 {
			tagRefNamespace = stream.Namespace
		}

		// different namespace
		if tagRefNamespace != stream.Namespace {
			glog.V(5).Infof("tagRefNamespace %q doesn't match stream namespace %q - skipping", tagRefNamespace, stream.Namespace)
			continue
		}

		// TODO: this is probably wrong - we should require ":<tag>", but we can't break old clients
		tagRefName := tagRef.From.Name
		parts := strings.Split(tagRefName, ":")
		tag := ""
		switch len(parts) {
		case 2:
			// <stream>:<tag>
			tagRefName = parts[0]
			tag = parts[1]
		default:
			// <tag> (this stream)
			tag = tagRefName
			tagRefName = stream.Name
		}

		glog.V(5).Infof("tagRefName=%q, tag=%q", tagRefName, tag)

		// different stream
		if tagRefName != stream.Name {
			glog.V(5).Infof("tagRefName %q doesn't match stream name %q - skipping", tagRefName, stream.Name)
			continue
		}

		// different tag
		if tag != updatedTag {
			glog.V(5).Infof("tag %q doesn't match updated tag %q - skipping", tag, updatedTag)
			continue
		}

		if AddTagEventToImageStream(stream, specTag, updatedImage) {
			glog.V(5).Infof("stream updated")
			updated++
		}
	}
	return updated
}

// ResolveImageID returns latest TagEvent for specified imageID and an error if
// there's more than one image matching the ID or when one does not exist.
func ResolveImageID(stream *ImageStream, imageID string) (*TagEvent, error) {
	var event *TagEvent
	set := sets.NewString()
	for _, history := range stream.Status.Tags {
		for i := range history.Items {
			tagging := &history.Items[i]
			if d, err := digest.ParseDigest(tagging.Image); err == nil {
				if strings.HasPrefix(d.Hex(), imageID) || strings.HasPrefix(tagging.Image, imageID) {
					event = tagging
					set.Insert(tagging.Image)
				}
				continue
			}
			if strings.HasPrefix(tagging.Image, imageID) {
				event = tagging
				set.Insert(tagging.Image)
			}
		}
	}
	switch len(set) {
	case 1:
		return &TagEvent{
			Created:              unversioned.Now(),
			DockerImageReference: event.DockerImageReference,
			Image:                event.Image,
		}, nil
	case 0:
		return nil, errors.NewNotFound(Resource("imagestreamimage"), imageID)
	default:
		return nil, errors.NewConflict(Resource("imagestreamimage"), imageID, fmt.Errorf("multiple images match the prefix %q: %s", imageID, strings.Join(set.List(), ", ")))
	}
}

// MostAccuratePullSpec returns a docker image reference that uses the current ID if possible, the current tag otherwise, and
// returns false if the reference if the spec could not be parsed. The returned spec has all client defaults applied.
func MostAccuratePullSpec(pullSpec string, id, tag string) (string, bool) {
	ref, err := ParseDockerImageReference(pullSpec)
	if err != nil {
		return pullSpec, false
	}
	if len(id) > 0 {
		ref.ID = id
	}
	if len(tag) > 0 {
		ref.Tag = tag
	}
	return ref.MostSpecific().Exact(), true
}

// ShortDockerImageID returns a short form of the provided DockerImage ID for display
func ShortDockerImageID(image *DockerImage, length int) string {
	id := image.ID
	if s, err := digest.ParseDigest(id); err == nil {
		id = s.Hex()
	}
	if len(id) > length {
		id = id[:length]
	}
	return id
}

// HasTagCondition returns true if the specified image stream tag has a condition with the same type, status, and
// reason (does not check generation, date, or message).
func HasTagCondition(stream *ImageStream, tag string, condition TagEventCondition) bool {
	for _, existing := range stream.Status.Tags[tag].Conditions {
		if condition.Type == existing.Type && condition.Status == existing.Status && condition.Reason == existing.Reason {
			return true
		}
	}
	return false
}

// SetTagConditions applies the specified conditions to the status of the given tag.
func SetTagConditions(stream *ImageStream, tag string, conditions ...TagEventCondition) {
	tagEvents := stream.Status.Tags[tag]
	tagEvents.Conditions = conditions
	if stream.Status.Tags == nil {
		stream.Status.Tags = make(map[string]TagEventList)
	}
	stream.Status.Tags[tag] = tagEvents
}

// LatestObservedTagGeneration returns the generation value for the given tag that has been observed by the controller
// monitoring the image stream. If the tag has not been observed, the generation is zero.
func LatestObservedTagGeneration(stream *ImageStream, tag string) int64 {
	tagEvents, ok := stream.Status.Tags[tag]
	if !ok {
		return 0
	}

	// find the most recent generation
	lastGen := int64(0)
	if items := tagEvents.Items; len(items) > 0 {
		tagEvent := items[0]
		if tagEvent.Generation > lastGen {
			lastGen = tagEvent.Generation
		}
	}
	for _, condition := range tagEvents.Conditions {
		if condition.Type != ImportSuccess {
			continue
		}
		if condition.Generation > lastGen {
			lastGen = condition.Generation
		}
		break
	}
	return lastGen
}

var (
	reMajorSemantic = regexp.MustCompile(`^[\d]+$`)
	reMinorSemantic = regexp.MustCompile(`^[\d]+\.[\d]+$`)
)

// PrioritizeTags orders a set of image tags with a few conventions:
//
// 1. the "latest" tag, if present, should be first
// 2. any tags that represent a semantic major version ("5", "v5") should be next, in descending order
// 3. any tags that represent a semantic minor version ("5.1", "v5.1") should be next, in descending order
// 4. any tags that represent a full semantic version ("5.1.3-other", "v5.1.3-other") should be next, in descending order
// 5. any remaining tags should be sorted in lexicographic order
//
// The method updates the tags in place.
func PrioritizeTags(tags []string) {
	remaining := tags
	finalTags := make([]string, 0, len(tags))
	for i, tag := range tags {
		if tag == DefaultImageTag {
			tags[0], tags[i] = tags[i], tags[0]
			finalTags = append(finalTags, tags[0])
			remaining = tags[1:]
			break
		}
	}

	exact := make(map[string]string)
	var major, minor, micro semver.Versions
	other := make([]string, 0, len(remaining))
	for _, tag := range remaining {
		short := strings.TrimLeft(tag, "v")
		v, err := semver.Parse(short)
		switch {
		case err == nil:
			exact[v.String()] = tag
			micro = append(micro, v)
			continue
		case reMajorSemantic.MatchString(short):
			if v, err = semver.Parse(short + ".0.0"); err == nil {
				exact[v.String()] = tag
				major = append(major, v)
				continue
			}
		case reMinorSemantic.MatchString(short):
			if v, err = semver.Parse(short + ".0"); err == nil {
				exact[v.String()] = tag
				minor = append(minor, v)
				continue
			}
		}
		other = append(other, tag)
	}
	sort.Sort(sort.Reverse(major))
	sort.Sort(sort.Reverse(minor))
	sort.Sort(sort.Reverse(micro))
	sort.Sort(sort.StringSlice(other))
	for _, v := range major {
		finalTags = append(finalTags, exact[v.String()])
	}
	for _, v := range minor {
		finalTags = append(finalTags, exact[v.String()])
	}
	for _, v := range micro {
		finalTags = append(finalTags, exact[v.String()])
	}
	for _, v := range other {
		finalTags = append(finalTags, v)
	}
	copy(tags, finalTags)
}

func ContainerImageEntrypointByAnnotation(annotations map[string]string, containerName string) ([]string, bool) {
	s, ok := annotations[fmt.Sprintf(containerImageEntrypointAnnotationFormatKey, containerName)]
	if !ok {
		return nil, false
	}
	var arr []string
	if err := json.Unmarshal([]byte(s), &arr); err != nil {
		return nil, false
	}
	return arr, true
}

func SetContainerImageEntrypointAnnotation(annotations map[string]string, containerName string, cmd []string) {
	key := fmt.Sprintf(containerImageEntrypointAnnotationFormatKey, containerName)
	if len(cmd) == 0 {
		delete(annotations, key)
		return
	}
	s, _ := json.Marshal(cmd)
	annotations[key] = string(s)
}
