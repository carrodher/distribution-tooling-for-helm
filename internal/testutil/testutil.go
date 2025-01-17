package testutil

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/Masterminds/sprig/v3"
	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"github.com/opencontainers/go-digest"
	"gopkg.in/yaml.v2"
)

var (
	tmplExtension    = ".tmpl"
	partialExtension = ".partial" + tmplExtension
)

var fns = template.FuncMap{
	"isLast": func(index int, len int) bool {
		return index+1 == len
	},
}

// RenderTemplateString renders a golang template defined in str with the provided tplData.
// It can receive an optional list of files to parse, including templates
func RenderTemplateString(str string, tplData interface{}, files ...string) (string, error) {
	tmpl := template.New("test")
	localFns := template.FuncMap{"include": func(name string, data interface{}) (string, error) {
		buf := bytes.NewBuffer(nil)
		if err := tmpl.ExecuteTemplate(buf, name, data); err != nil {
			return "", err
		}
		return buf.String(), nil
	},
	}

	tmpl, err := tmpl.Funcs(fns).Funcs(sprig.FuncMap()).Funcs(localFns).Parse(str)
	if err != nil {
		return "", err
	}
	if len(files) > 0 {
		if _, err := tmpl.ParseFiles(files...); err != nil {
			return "", err
		}
	}
	b := &bytes.Buffer{}

	if err := tmpl.Execute(b, tplData); err != nil {
		return "", err
	}
	return strings.TrimSpace(b.String()), nil
}

// RenderTemplateFile renders the golang template specified in file with the provided tplData.
// It can receive an optional list of files to parse, including templates
func RenderTemplateFile(file string, tplData interface{}, files ...string) (string, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return "", err
	}
	return RenderTemplateString(string(data), tplData, files...)
}

// RenderScenario renders a full directory specified by origin in the destDir directory with
// the specified data
func RenderScenario(origin string, destDir string, data map[string]interface{}) error {
	matches, err := filepath.Glob(origin)
	if err != nil {
		return err
	}
	if len(matches) == 0 {
		return fmt.Errorf("cannot find any files at %q", origin)
	}
	templateFiles, err := filepath.Glob(filepath.Join(origin, fmt.Sprintf("*%s", partialExtension)))
	_ = templateFiles
	if err != nil {
		return fmt.Errorf("faled to list template partials")
	}
	for _, p := range matches {
		rootDir := filepath.Dir(filepath.Clean(p))
		err := filepath.Walk(p, func(path string, info os.FileInfo, err error) error {
			if strings.HasSuffix(path, partialExtension) {
				return nil
			}
			relative, _ := filepath.Rel(rootDir, path)
			destFile := filepath.Join(destDir, relative)

			if info.Mode().IsRegular() {
				if strings.HasSuffix(path, tmplExtension) {
					destFile = strings.TrimSuffix(destFile, tmplExtension)
					rendered, err := RenderTemplateFile(path, data, templateFiles...)
					if err != nil {
						return fmt.Errorf("failed to render template %q: %v", path, err)
					}

					if err := os.WriteFile(destFile, []byte(rendered), 0644); err != nil {
						return err
					}
				} else {
					err := copyFile(path, destFile)
					if err != nil {
						return fmt.Errorf("failed to copy %q: %v", path, err)
					}
				}
			} else if info.IsDir() {
				if err := os.MkdirAll(destFile, info.Mode()); err != nil {
					return fmt.Errorf("failed to create directory: %v", err)
				}
			} else {
				return fmt.Errorf("unknown file type (%s)", path)
			}
			if err := os.Chmod(destFile, info.Mode().Perm()); err != nil {
				log.Printf("DEBUG: failed to change file %q permissions: %v", destFile, err)
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}

type sampleImageData struct {
	Index     v1.ImageIndex
	ImageData ImageData
}

func createSampleImages(imageName string, server string) (map[string]sampleImageData, error) {
	images := make(map[string]sampleImageData, 0)
	src := fmt.Sprintf("%s/%s", server, imageName)
	imageData := ImageData{Name: "test", Image: imageName}

	addendums := []mutate.IndexAddendum{}

	for _, plat := range []string{
		"linux/amd64",
		"linux/arm64",
	} {
		img, err := crane.Image(map[string][]byte{
			"platform.txt": []byte(fmt.Sprintf("Image: %s ; plaform: %s", imageName, plat)),
		})
		if err != nil {
			return nil, fmt.Errorf("failed to create image: %v", err)
		}

		parts := strings.Split(plat, "/")
		addendums = append(addendums, mutate.IndexAddendum{
			Add: img,
			Descriptor: v1.Descriptor{
				Platform: &v1.Platform{
					OS:           parts[0],
					Architecture: parts[1],
				},
			},
		})
		d, err := img.Digest()
		if err != nil {
			return nil, fmt.Errorf("failed to generate digest: %v", err)
		}
		imageData.Digests = append(imageData.Digests, DigestData{Arch: plat, Digest: digest.Digest(d.String())})
	}

	idx := mutate.AppendManifests(empty.Index, addendums...)

	images[src] = sampleImageData{Index: idx, ImageData: imageData}
	return images, nil
}

// AddSampleImagesToRegistry adds a set of sample images to the provided registry
func AddSampleImagesToRegistry(imageName string, server string) ([]ImageData, error) {
	images := make([]ImageData, 0)
	samples, err := createSampleImages(imageName, server)
	if err != nil {
		return nil, err
	}

	for src, data := range samples {
		ref, err := name.ParseReference(src)
		if err != nil {
			return nil, fmt.Errorf("failed to parse reference: %v", err)
		}
		if err := remote.WriteIndex(ref, data.Index); err != nil {
			return nil, fmt.Errorf("failed to write index: %v", err)
		}
		images = append(images, data.ImageData)
	}
	return images, nil
}

// CreateSingleArchImage creates a sample image for the specified platform
func CreateSingleArchImage(imageData *ImageData, plat string) (v1.Image, error) {
	imageName := imageData.Image

	img, err := crane.Image(map[string][]byte{
		"platform.txt": []byte(fmt.Sprintf("Image: %s ; plaform: %s", imageName, plat)),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create image: %w", err)
	}
	parts := strings.Split(plat, "/")
	img, err = mutate.ConfigFile(img, &v1.ConfigFile{Architecture: parts[1], OS: parts[0]})
	if err != nil {
		return nil, fmt.Errorf("cannot mutatle image config file: %w", err)
	}

	img, err = mutate.Canonical(img)
	if err != nil {
		return nil, fmt.Errorf("failed to canonicalize image: %w", err)
	}
	d, err := img.Digest()
	if err != nil {
		return nil, fmt.Errorf("failed to get image digest: %w", err)
	}
	imageData.Digests = append(imageData.Digests, DigestData{Arch: plat, Digest: digest.Digest(d.String())})

	return img, nil
}

// CreateSampleImages create a multiplatform sample image
func CreateSampleImages(imageData *ImageData, archs []string) ([]v1.Image, error) {
	craneImgs := []v1.Image{}

	for _, plat := range archs {
		img, err := CreateSingleArchImage(imageData, plat)
		if err != nil {
			return nil, err
		}
		craneImgs = append(craneImgs, img)
	}
	return craneImgs, nil
}

// ReadRemoteImageManifest reads the image src digests from a remote repository
func ReadRemoteImageManifest(src string) (map[string]DigestData, error) {
	o := crane.GetOptions()

	ref, err := name.ParseReference(src, o.Name...)

	if err != nil {
		return nil, fmt.Errorf("failed to parse reference %q: %w", src, err)
	}
	desc, err := remote.Get(ref, o.Remote...)
	if err != nil {
		return nil, fmt.Errorf("failed to get remote image: %w", err)
	}

	var idx v1.IndexManifest
	if err := json.Unmarshal(desc.Manifest, &idx); err != nil {
		return nil, fmt.Errorf("failed to parse images data")
	}
	digests := make(map[string]DigestData, 0)

	var allErrors error
	for _, img := range idx.Manifests {
		// Skip attestations
		if img.Annotations["vnd.docker.reference.type"] == "attestation-manifest" {
			continue
		}
		switch img.MediaType {
		case types.OCIManifestSchema1, types.DockerManifestSchema2:
			if img.Platform == nil {
				continue
			}

			arch := fmt.Sprintf("%s/%s", img.Platform.OS, img.Platform.Architecture)
			imgDigest := DigestData{
				Digest: digest.Digest(img.Digest.String()),
				Arch:   arch,
			}
			digests[arch] = imgDigest
		default:
			allErrors = errors.Join(allErrors, fmt.Errorf("unknown media type %q", img.MediaType))
			continue
		}
	}
	return digests, allErrors
}

// MustNormalizeYAML returns the normalized version of the text YAML or panics
func MustNormalizeYAML(text string) string {
	t, err := NormalizeYAML(text)
	if err != nil {
		panic(err)
	}
	return t
}

// NormalizeYAML returns a normalized version of the provided YAML text
func NormalizeYAML(text string) (string, error) {
	var out interface{}
	err := yaml.Unmarshal([]byte(text), &out)
	if err != nil {
		return "", err
	}
	data, err := yaml.Marshal(out)
	return string(data), err
}
