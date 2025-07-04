// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"

	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ecr"
	"github.com/awslabs/soci-snapshotter/soci/store"
	"github.com/containerd/containerd/images"

	"slices"

	"github.com/aws-ia/cfn-aws-soci-index-builder/soci-index-generator-lambda/utils/log"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

const (
	MediaTypeDockerManifestList = "application/vnd.docker.distribution.manifest.list.v2+json"
	MediaTypeDockerManifest     = "application/vnd.docker.distribution.manifest.v2+json"
	MediaTypeOCIManifest        = "application/vnd.oci.image.manifest.v1+json"
	MediaTypeOCIImageIndex      = "application/vnd.oci.image.index.v1+json"

	MediaTypeDockerImageConfig = "application/vnd.docker.container.image.v1+json"
	MediaTypeOCIImageConfig    = "application/vnd.oci.image.config.v1+json"
)

// List of config's media type for images
var ImageConfigMediaTypes = []string{MediaTypeDockerImageConfig, MediaTypeOCIImageConfig}

type Registry struct {
	registry *remote.Registry
}

var RegistryNotSupportingOciArtifacts = errors.New("Registry does not support OCI artifacts")

// Initialize a remote registry
func Init(ctx context.Context, registryUrl string) (*Registry, error) {
	log.Info(ctx, "Initializing registry client")
	registry, err := remote.NewRegistry(registryUrl)
	if err != nil {
		return nil, err
	}
	if isEcrRegistry(registryUrl) {
		err := authorizeEcr(registry)
		if err != nil {
			return nil, err
		}
	}
	return &Registry{registry}, nil
}

// Pull an image from the remote registry to a local OCI Store
// imageReference can be either a digest or a tag
func (registry *Registry) Pull(ctx context.Context, repositoryName string, sociStore *store.SociStore, imageReference string) (*ocispec.Descriptor, error) {
	log.Info(ctx, "Pulling image")
	repo, err := registry.registry.Repository(ctx, repositoryName)
	if err != nil {
		return nil, err
	}

	imageDescriptor, err := oras.Copy(ctx, repo, imageReference, sociStore, imageReference, oras.DefaultCopyOptions)
	if err != nil {
		return nil, err
	}

	return &imageDescriptor, nil
}

// Push a OCI artifact to remote registry
// descriptor: ocispec Descriptor of the artifact
// ociStore: the local OCI store
// tag: optional tag to apply to the artifact (empty string means no tag)
func (registry *Registry) Push(ctx context.Context, sociStore *store.SociStore, indexDesc ocispec.Descriptor, repositoryName string, tag string) error {
	log.Info(ctx, "Pushing artifact")

	repo, err := registry.registry.Repository(ctx, repositoryName)
	if err != nil {
		return err
	}

	err = oras.CopyGraph(ctx, sociStore, repo, indexDesc, oras.DefaultCopyGraphOptions)
	if err != nil {
		// TODO: There might be a better way to check if a registry supporting OCI or not
		if strings.Contains(err.Error(), "Response status code 405: unsupported: Invalid parameter at 'ImageManifest' failed to satisfy constraint: 'Invalid JSON syntax'") {
			log.Warn(ctx, fmt.Sprintf("Error when pushing: %v", err))
			return RegistryNotSupportingOciArtifacts
		}
		return err
	}

	// If a tag is provided, tag the artifact in the remote repository
	if tag != "" {
		log.Info(ctx, fmt.Sprintf("Tagging index with %s", tag))
		err = repo.Tag(ctx, indexDesc, tag)
		if err != nil {
			return fmt.Errorf("failed to tag artifact: %w", err)
		}
	}

	return nil
}

// Call registry's headManifest and return the manifest's descriptor
func (registry *Registry) HeadManifest(ctx context.Context, repositoryName string, reference string) (ocispec.Descriptor, error) {
	repo, err := registry.registry.Repository(ctx, repositoryName)
	if err != nil {
		return ocispec.Descriptor{}, err
	}

	descriptor, err := repo.Resolve(ctx, reference)
	if err != nil {
		return descriptor, err
	}

	return descriptor, nil
}

// Call registry's getManifest and return the image's manifest
// The image reference must be a digest because that's what oras-go FetchReference takes
func (registry *Registry) GetManifest(ctx context.Context, repositoryName string, digest string) (ocispec.Manifest, error) {
	repo, err := registry.registry.Repository(ctx, repositoryName)
	var manifest ocispec.Manifest
	if err != nil {
		return manifest, err
	}

	_, rc, err := repo.FetchReference(ctx, digest)
	if err != nil {
		return manifest, err
	}

	bytes, err := io.ReadAll(rc)
	if err != nil {
		return manifest, err
	}

	err = json.Unmarshal(bytes, &manifest)
	if err != nil {
		return manifest, err
	}

	return manifest, nil
}

func (registry *Registry) validateImageManifest(ctx context.Context, repositoryName string, digest string) error {
	// Get the manifest content
	manifest, err := registry.GetManifest(ctx, repositoryName, digest)
	if err != nil {
		return err
	}

	// Valid image manifests must have a config with a valid media type
	if manifest.Config.MediaType == "" {
		return fmt.Errorf("not a valid image manifest: empty config media type")
	}

	if !slices.Contains(ImageConfigMediaTypes, manifest.Config.MediaType) {
		return fmt.Errorf("not a valid image manifest: unexpected config media type: %s, expected one of: %v",
			manifest.Config.MediaType, ImageConfigMediaTypes)
	}

	return nil
}

func (registry *Registry) validateImageIndex(ctx context.Context, repositoryName string, digest string) error {
	// Get the descriptor to check media type
	descriptor, err := registry.HeadManifest(ctx, repositoryName, digest)
	if err != nil {
		return err
	}

	// Check if it's an image index by media type
	if !images.IsIndexType(descriptor.MediaType) {
		return fmt.Errorf("not a valid image index: unexpected media type: %s", descriptor.MediaType)
	}

	return nil
}

// ValidateImageDigest validates if a digest is valid based on SOCI index version requirements
// For SOCI V1, only image manifests are supported
// For SOCI V2, both image manifests and image indexes are supported
func (registry *Registry) ValidateImageDigest(ctx context.Context, repositoryName string, digest string, sociIndexVersion string) error {
	var err error
	if sociIndexVersion == "V1" {
		err = registry.validateImageManifest(ctx, repositoryName, digest)
		if err != nil {
			return err
		}
		log.Info(ctx, "Validated image manifest")
	}
	if sociIndexVersion == "V2" {
		err = registry.validateImageIndex(ctx, repositoryName, digest)
		if err == nil {
			log.Info(ctx, "Validated image index")
			return nil
		}
		err = registry.validateImageManifest(ctx, repositoryName, digest)
		if err == nil {
			log.Info(ctx, "Validated image manifest")
			return nil
		}
	}
	return err
}

// Check if a registry is an ECR registry
func isEcrRegistry(registryUrl string) bool {
	ecrRegistryUrlRegex := "\\d{12}\\.dkr\\.ecr\\.\\S+\\.amazonaws\\.com"
	match, err := regexp.MatchString(ecrRegistryUrlRegex, registryUrl)
	if err != nil {
		panic(err)
	}
	return match
}

// Authorize ECR registry
func authorizeEcr(ecrRegistry *remote.Registry) error {
	// getting ecr auth token
	input := &ecr.GetAuthorizationTokenInput{}
	var ecrClient *ecr.ECR
	ecrEndpoint := os.Getenv("ECR_ENDPOINT") // set this env var for custom, i.e. non default, aws ecr endpoint
	if ecrEndpoint != "" {
		ecrClient = ecr.New(session.New(&aws.Config{Endpoint: aws.String(ecrEndpoint)}))
	} else {
		ecrClient = ecr.New(session.New())
	}
	getAuthorizationTokenResponse, err := ecrClient.GetAuthorizationToken(input)
	if err != nil {
		return err
	}

	if len(getAuthorizationTokenResponse.AuthorizationData) == 0 {
		return errors.New("Couldn't authorize with ECR: empty authorization data returned")
	}

	ecrAuthorizationToken := getAuthorizationTokenResponse.AuthorizationData[0].AuthorizationToken
	if len(*ecrAuthorizationToken) == 0 {
		return errors.New("Couldn't authorize with ECR: empty authorization token returned")
	}

	ecrRegistry.RepositoryOptions.Client = &auth.Client{
		Header: http.Header{
			"Authorization": {"Basic " + *ecrAuthorizationToken},
			"User-Agent":    {"SOCI Index Builder (oras-go)"},
		},
	}
	return nil
}
