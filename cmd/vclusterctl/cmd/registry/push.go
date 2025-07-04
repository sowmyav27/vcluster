package registry

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/docker/docker/pkg/jsonmessage"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/loft-sh/log"
	"github.com/loft-sh/vcluster/pkg/cli/flags"
	"github.com/loft-sh/vcluster/pkg/util/archive"
	"github.com/spf13/cobra"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

type PushOptions struct {
	*flags.GlobalFlags

	Images   []string
	Archives []string

	Log log.Logger
}

func NewPushCmd(globalFlags *flags.GlobalFlags) *cobra.Command {
	o := &PushOptions{
		GlobalFlags: globalFlags,

		Log: log.GetInstance(),
	}

	cmd := &cobra.Command{
		Use:   "push",
		Short: "Push a local docker image or archive into vCluster registry",
		RunE: func(cmd *cobra.Command, args []string) error {
			return o.Run(cmd.Context(), args)
		},
	}

	cmd.Flags().StringSliceVar(&o.Archives, "archive", []string{}, "Path to the archive.tar or archive.tar.gz file. Can also be a directory with .tar or .tar.gz files.")

	return cmd
}

func (o *PushOptions) Run(ctx context.Context, args []string) error {
	if len(args) > 0 {
		o.Images = args
	}

	if len(o.Images) == 0 && len(o.Archives) == 0 {
		return fmt.Errorf("either image or --archive is required")
	}

	// get the client config
	restConfig, err := getClient(o.GlobalFlags)
	if err != nil {
		return fmt.Errorf("failed to get client config: %w", err)
	}

	// get the transport
	transport, err := rest.TransportFor(restConfig)
	if err != nil {
		return fmt.Errorf("failed to get transport: %w", err)
	}

	// check if registry is enabled
	registryEnabled, err := isRegistryEnabled(ctx, restConfig.Host, transport)
	if err != nil {
		return fmt.Errorf("failed to check if registry is enabled: %w", err)
	} else if !registryEnabled {
		return fmt.Errorf("vCluster registry is not enabled or the target cluster is not a vCluster. Please make sure to enable the registry in the vCluster config and run `vcluster connect` to connect to the vCluster before pushing images")
	}

	// save image to archive
	if len(o.Images) > 0 {
		o.Log.Infof("Saving image(s) %s to archive...", strings.Join(o.Images, ", "))

		args := []string{"docker", "save", "-o", "image.tar"}
		args = append(args, o.Images...)
		if err := runCommand(args...); err != nil {
			return fmt.Errorf("failed to save image: %w", err)
		}

		// cleanup
		defer os.Remove("image.tar")
		o.Archives = append(o.Archives, "image.tar")
	}

	// get the vCluster host
	vClusterHost, err := url.Parse(restConfig.Host)
	if err != nil {
		return fmt.Errorf("failed to parse vCluster host: %w", err)
	}

	// get the remote registry
	remoteRegistry, err := name.NewRegistry(vClusterHost.Host)
	if err != nil {
		return fmt.Errorf("failed to get remote registry: %w", err)
	}

	// try to push the image to the remote registry
	for _, archive := range o.Archives {
		stat, err := os.Stat(archive)
		if err != nil {
			return fmt.Errorf("failed to stat archive: %w", err)
		}

		// if the archive is a directory, push all tar and tar.gz files in the directory
		if stat.IsDir() {
			files, err := os.ReadDir(archive)
			if err != nil {
				return fmt.Errorf("failed to read directory: %w", err)
			}

			// push all tar and tar.gz files in the directory
			for _, file := range files {
				if !strings.HasSuffix(file.Name(), ".tar") && !strings.HasSuffix(file.Name(), ".tar.gz") {
					continue
				}

				if err := o.copyArchiveToRemote(ctx, filepath.Join(archive, file.Name()), remoteRegistry, transport); err != nil {
					return err
				}
			}
		} else if err := o.copyArchiveToRemote(ctx, archive, remoteRegistry, transport); err != nil {
			return err
		}
	}

	return nil
}

func isRegistryEnabled(ctx context.Context, host string, transport http.RoundTripper) (bool, error) {
	client := &http.Client{Transport: transport}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/v2/", host), nil)
	if err != nil {
		return false, fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return false, fmt.Errorf("failed request: %w", err)
	}
	defer resp.Body.Close()

	return resp.StatusCode == http.StatusOK, nil
}

func runCommand(args ...string) error {
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func (o *PushOptions) copyArchiveToRemote(ctx context.Context, path string, remoteRegistry name.Registry, transport http.RoundTripper) error {
	// create a temp directory to extract the archive to
	tempDir, err := os.MkdirTemp("", "vcluster-image")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tempDir)

	// extract the archive to the temp directory
	if strings.HasSuffix(path, ".tar.gz") {
		if err := archive.ExtractTarGz(path, tempDir); err != nil {
			return err
		}
	} else if strings.HasSuffix(path, ".tar") {
		if err := archive.ExtractTar(path, tempDir); err != nil {
			return err
		}
	} else {
		return fmt.Errorf("unsupported archive format: %s, must be .tar or .tar.gz", path)
	}

	// get the image index from the temp directory
	imageIndex, err := layout.ImageIndexFromPath(tempDir)
	if err != nil {
		return err
	}

	manifest, err := imageIndex.IndexManifest()
	if err != nil {
		return err
	}

	for _, manifest := range manifest.Manifests {
		imageTag := manifest.Annotations["io.containerd.image.name"]
		if imageTag == "" {
			return fmt.Errorf("image tag not found in manifest: %s, annotations: %v", path, manifest.Annotations)
		}

		localRef, err := name.ParseReference(imageTag)
		if err != nil {
			return fmt.Errorf("failed to parse image reference: %w", err)
		}

		image, err := imageIndex.Image(manifest.Digest)
		if err != nil {
			return fmt.Errorf("failed to get image: %w", err)
		}

		if err := o.pushImage(ctx, localRef, image, remoteRegistry, transport); err != nil {
			return err
		}
	}

	return nil
}

func (o *PushOptions) pushImage(ctx context.Context, localRef name.Reference, image v1.Image, remoteRegistry name.Registry, transport http.RoundTripper) error {
	remoteRef, err := replaceRegistry(localRef, remoteRegistry)
	if err != nil {
		return err
	}

	o.Log.Infof("Pushing image %s to %s", localRef.String(), remoteRef.String())
	progressChan := make(chan v1.Update, 200)
	errChan := make(chan error, 1)

	// push image to remote registry
	go func() {
		errChan <- remote.Push(
			remoteRef,
			image,
			remote.WithContext(ctx),
			remote.WithProgress(progressChan),
			remote.WithTransport(transport),
		)
	}()

	for update := range progressChan {
		if update.Error != nil {
			return update.Error
		}

		status := "Pushing"
		if update.Complete == update.Total {
			status = "Pushed"
		}

		_, err := fmt.Fprintf(os.Stdout, "%s %s\n", status, (&jsonmessage.JSONProgress{
			Current: update.Complete,
			Total:   update.Total,
		}).String())
		if err != nil {
			return err
		}
	}

	err = <-errChan
	if err != nil {
		return err
	}

	o.Log.Infof("Successfully pushed image %s", remoteRef.String())
	return nil
}

func replaceRegistry(localRef name.Reference, remoteRegistry name.Registry) (name.Reference, error) {
	localTag, err := name.NewTag(localRef.Name())
	if err != nil {
		return nil, fmt.Errorf("failed to parse tag: %w", err)
	}
	remoteTag := localTag
	remoteTag.Repository.Registry = remoteRegistry
	remoteRef, err := name.ParseReference(remoteTag.Name())
	if err != nil {
		return nil, fmt.Errorf("failed to parse remote reference: %w", err)
	}

	return remoteRef, nil
}

func getClient(flags *flags.GlobalFlags) (*rest.Config, error) {
	// first load the kube config
	kubeClientConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(clientcmd.NewDefaultClientConfigLoadingRules(), &clientcmd.ConfigOverrides{
		CurrentContext: flags.Context,
	})

	// get the client config
	restConfig, err := kubeClientConfig.ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to get client config: %w", err)
	}

	return restConfig, nil
}
