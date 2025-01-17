package chartutils

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/partial"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"github.com/vmware-labs/distribution-tooling-for-helm/imagelock"
	"github.com/vmware-labs/distribution-tooling-for-helm/utils"
)

func getNumberOfArtifacts(images imagelock.ImageList) int {
	n := 0
	for _, imgDesc := range images {
		n += len(imgDesc.Digests)
	}
	return n
}

// PullImages downloads the list of images specified in the provided ImagesLock
func PullImages(lock *imagelock.ImagesLock, imagesDir string, opts ...Option) error {

	cfg := NewConfiguration(opts...)
	ctx := cfg.Context
	o := crane.GetOptions(crane.WithContext(ctx))

	if err := os.MkdirAll(imagesDir, 0755); err != nil {
		return fmt.Errorf("failed to create bundle directory: %v", err)
	}
	l := cfg.Log

	p, _ := cfg.ProgressBar.WithTotal(getNumberOfArtifacts(lock.Images)).UpdateTitle("Pulling Images").Start()
	defer p.Stop()
	maxRetries := cfg.MaxRetries

	for _, imgDesc := range lock.Images {
		for _, dgst := range imgDesc.Digests {
			select {
			// Early abort if the context is done
			case <-ctx.Done():
				return fmt.Errorf("cancelled execution")
			default:
				p.Add(1)
				p.UpdateTitle(fmt.Sprintf("Saving image %s/%s %s (%s)", imgDesc.Chart, imgDesc.Name, imgDesc.Image, dgst.Arch))
				err := utils.ExecuteWithRetry(maxRetries, func(try int, prevErr error) error {
					if try > 0 {
						// The context is done, so we are not retrying, just return the error
						if ctx.Err() != nil {
							return prevErr
						}
						l.Debugf("Failed to pull image: %v", prevErr)
						p.Warnf("Failed to pull image: retrying %d/%d", try, maxRetries)
					}
					if _, err := pullImage(imgDesc.Image, dgst, imagesDir, o); err != nil {
						return err
					}
					return nil
				})

				if err != nil {
					return fmt.Errorf("failed to pull image %q: %w", imgDesc.Name, err)
				}
			}
		}

	}
	return nil
}

// PushImages push the list of images in imagesDir to the destination specified in the ImagesLock
func PushImages(lock *imagelock.ImagesLock, imagesDir string, opts ...Option) error {
	cfg := NewConfiguration(opts...)
	l := cfg.Log

	ctx := cfg.Context

	p, _ := cfg.ProgressBar.WithTotal(len(lock.Images)).UpdateTitle("Pushing Images").Start()
	defer p.Stop()

	o := crane.GetOptions(crane.WithContext(ctx))

	maxRetries := cfg.MaxRetries
	for _, imgData := range lock.Images {
		select {
		// Early abort if the context is done
		case <-ctx.Done():
			return fmt.Errorf("cancelled execution")
		default:
			p.Add(1)
			p.UpdateTitle(fmt.Sprintf("Pushing image %q", imgData.Image))
			err := utils.ExecuteWithRetry(maxRetries, func(try int, prevErr error) error {
				if try > 0 {
					// The context is done, so we are not retrying, just return the error
					if ctx.Err() != nil {
						return prevErr
					}
					l.Debugf("Failed to push image: %v", prevErr)
					p.Warnf("Failed to push image: retrying %d/%d", try, maxRetries)
				}
				return pushImage(imgData, imagesDir, o)
			})
			if err != nil {
				return fmt.Errorf("failed to push image %q: %w", imgData.Name, err)
			}
		}
	}
	return nil
}

func buildImageIndex(image *imagelock.ChartImage, imagesDir string) (v1.ImageIndex, error) {
	adds := make([]mutate.IndexAddendum, 0, len(image.Digests))

	base := mutate.IndexMediaType(empty.Index, types.DockerManifestList)
	for _, dgstData := range image.Digests {
		imgFileName := getImageTarFile(imagesDir, dgstData)

		img, err := crane.Load(imgFileName)
		if err != nil {
			return nil, fmt.Errorf("loading %s as tarball: %w", imgFileName, err)
		}

		newDesc, err := partial.Descriptor(img)
		if err != nil {
			return nil, fmt.Errorf("failed to create descriptor: %w", err)
		}
		cf, err := img.ConfigFile()
		if err != nil {
			return nil, fmt.Errorf("failed to obtain image config file: %w", err)
		}
		newDesc.Platform = cf.Platform()
		adds = append(adds, mutate.IndexAddendum{
			Add:        img,
			Descriptor: *newDesc,
		})
	}
	return mutate.AppendManifests(base, adds...), nil
}

func pushImage(imgData *imagelock.ChartImage, imagesDir string, o crane.Options) error {
	idx, err := buildImageIndex(imgData, imagesDir)
	if err != nil {
		return fmt.Errorf("failed to build image index: %w", err)
	}

	ref, err := name.ParseReference(imgData.Image, o.Name...)
	if err != nil {
		return fmt.Errorf("failed to parse image reference %q: %w", imgData.Image, err)
	}

	if err := remote.WriteIndex(ref, idx, o.Remote...); err != nil {
		return fmt.Errorf("failed to write image index: %w", err)
	}

	return nil
}

func getImageTarFile(imagesDir string, dgst imagelock.DigestInfo) string {
	return filepath.Join(imagesDir, fmt.Sprintf("%s.tar", dgst.Digest.Encoded()))
}

func pullImage(image string, digest imagelock.DigestInfo, imagesDir string, o crane.Options) (string, error) {
	imgFileName := getImageTarFile(imagesDir, digest)

	src := fmt.Sprintf("%s@%s", image, digest.Digest)
	ref, err := name.ParseReference(src, o.Name...)
	if err != nil {
		return "", fmt.Errorf("parsing reference %q: %w", src, err)
	}

	rmt, err := remote.Get(ref, o.Remote...)
	if err != nil {
		return "", err
	}
	img, err := rmt.Image()
	if err != nil {
		return "", err
	}

	if err := crane.Save(img, image, imgFileName); err != nil {
		return "", fmt.Errorf("failed to save image %q to %q: %w", image, imgFileName, err)
	}
	return imgFileName, nil
}
