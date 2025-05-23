/*
   Copyright 2020 Docker Compose CLI authors

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

package remote

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/compose-spec/compose-go/v2/loader"
	"github.com/distribution/reference"
	"github.com/docker/buildx/store/storeutil"
	"github.com/docker/buildx/util/imagetools"
	"github.com/docker/cli/cli/command"
	"github.com/docker/compose/v2/internal/ocipush"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

const (
	OCI_REMOTE_ENABLED = "COMPOSE_EXPERIMENTAL_OCI_REMOTE"
	OciPrefix          = "oci://"
)

func ociRemoteLoaderEnabled() (bool, error) {
	if v := os.Getenv(OCI_REMOTE_ENABLED); v != "" {
		enabled, err := strconv.ParseBool(v)
		if err != nil {
			return false, fmt.Errorf("COMPOSE_EXPERIMENTAL_OCI_REMOTE environment variable expects boolean value: %w", err)
		}
		return enabled, err
	}
	return true, nil
}

func NewOCIRemoteLoader(dockerCli command.Cli, offline bool) loader.ResourceLoader {
	return ociRemoteLoader{
		dockerCli: dockerCli,
		offline:   offline,
		known:     map[string]string{},
	}
}

type ociRemoteLoader struct {
	dockerCli command.Cli
	offline   bool
	known     map[string]string
}

func (g ociRemoteLoader) Accept(path string) bool {
	return strings.HasPrefix(path, OciPrefix)
}

func (g ociRemoteLoader) Load(ctx context.Context, path string) (string, error) {
	enabled, err := ociRemoteLoaderEnabled()
	if err != nil {
		return "", err
	}
	if !enabled {
		return "", fmt.Errorf("OCI remote resource is disabled by %q", OCI_REMOTE_ENABLED)
	}

	if g.offline {
		return "", nil
	}

	local, ok := g.known[path]
	if !ok {
		ref, err := reference.ParseDockerRef(path[len(OciPrefix):])
		if err != nil {
			return "", err
		}

		opt, err := storeutil.GetImageConfig(g.dockerCli, nil)
		if err != nil {
			return "", err
		}
		resolver := imagetools.New(opt)

		content, descriptor, err := resolver.Get(ctx, ref.String())
		if err != nil {
			return "", err
		}

		cache, err := cacheDir()
		if err != nil {
			return "", fmt.Errorf("initializing remote resource cache: %w", err)
		}

		local = filepath.Join(cache, descriptor.Digest.Hex())
		if _, err = os.Stat(local); os.IsNotExist(err) {
			var manifest v1.Manifest
			err = json.Unmarshal(content, &manifest)
			if err != nil {
				return "", err
			}

			err = g.pullComposeFiles(ctx, local, manifest, ref, resolver)
			if err != nil {
				// we need to clean up the directory to be sure we won't let empty files present
				_ = os.RemoveAll(local)
				return "", err
			}
		}
		g.known[path] = local
	}
	return filepath.Join(local, "compose.yaml"), nil
}

func (g ociRemoteLoader) Dir(path string) string {
	return g.known[path]
}

func (g ociRemoteLoader) pullComposeFiles(ctx context.Context, local string, manifest v1.Manifest, ref reference.Named, resolver *imagetools.Resolver) error { //nolint:gocyclo
	err := os.MkdirAll(local, 0o700)
	if err != nil {
		return err
	}
	composeFile := filepath.Join(local, "compose.yaml")
	f, err := os.Create(composeFile)
	if err != nil {
		return err
	}
	defer f.Close() //nolint:errcheck
	if (manifest.ArtifactType != "" && manifest.ArtifactType != ocipush.ComposeProjectArtifactType) ||
		(manifest.ArtifactType == "" && manifest.Config.MediaType != ocipush.ComposeEmptyConfigMediaType) {
		return fmt.Errorf("%s is not a compose project OCI artifact, but %s", ref.String(), manifest.ArtifactType)
	}

	for i, layer := range manifest.Layers {
		digested, err := reference.WithDigest(ref, layer.Digest)
		if err != nil {
			return err
		}
		content, _, err := resolver.Get(ctx, digested.String())
		if err != nil {
			return err
		}

		switch layer.MediaType {
		case ocipush.ComposeYAMLMediaType:
			target := f
			_, extends := layer.Annotations["com.docker.compose.extends"]
			if extends {
				target, err = os.Create(filepath.Join(local, layer.Annotations["com.docker.compose.file"]))
				if err != nil {
					return err
				}
			}
			if err := writeComposeFile(layer, i, target, content); err != nil {
				return err
			}
		case ocipush.ComposeEnvFileMediaType:
			if err := writeEnvFile(layer, local, content); err != nil {
				return err
			}
		case ocipush.ComposeEmptyConfigMediaType:
		}
	}
	return nil
}

func writeComposeFile(layer v1.Descriptor, i int, f *os.File, content []byte) error {
	if _, ok := layer.Annotations["com.docker.compose.file"]; i > 0 && ok {
		_, err := f.Write([]byte("\n---\n"))
		if err != nil {
			return err
		}
	}
	_, err := f.Write(content)
	return err
}

func writeEnvFile(layer v1.Descriptor, local string, content []byte) error {
	envfilePath, ok := layer.Annotations["com.docker.compose.envfile"]
	if !ok {
		return fmt.Errorf("missing annotation com.docker.compose.envfile in layer %q", layer.Digest)
	}
	otherFile, err := os.Create(filepath.Join(local, envfilePath))
	if err != nil {
		return err
	}
	_, err = otherFile.Write(content)
	if err != nil {
		return err
	}
	return nil
}

var _ loader.ResourceLoader = ociRemoteLoader{}
