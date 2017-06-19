#!/usr/bin/env python2

import sys
import shutil
import distutils.dir_util as dir_util
import subprocess
import tempfile

import os
import os.path
import argparse
import logging


LOG = logging.getLogger("hack/build-local-images")
if os.getenv('OS_DEBUG'):
    logging.basicConfig(level='DEBUG')
else:
    logging.basicConfig(level='INFO')


class ImageManager(object):
    UNPREFIXED_IMAGES = set(["node, openvswitch"])
    CONFIG_DEFAULTS = {
        'binaries': {'openshift': '/usr/bin/openshift'},
        'files': {},
    }
    BASE_IMAGE_CONFIG = {
        "directory": "origin",
        "binaries": {"openshift": "/usr/bin/openshift"},
    }
    IMAGE_CONFIGS = {
        "deployer": {"directory": "deployer"},
        "recycler": {"directory": "recycler"},
        "docker-builder": {"directory": "builder/docker/docker-builder"},
        "sti-builder": {"directory": "builder/docker/sti-builder"},
        "f5-router": {"directory": "router/f5"},
        "haproxy-router": {
            "directory": "router/haproxy",
            "files": {
                ".": "/var/lib/haproxy"
            }
        },
        "keepalived-ipfailover": {
            "directory": "ipfailover/keepalived",
            "files": {
                ".": "/var/lib/ipfailover/keepalived"
            }
        },
        "node": {"directory": "node"},
        "openvswitch": {"directory": "openvswitch"},
    }

    def __init__(self, requested_images=[], prefix=None):
        self._images = set(requested_images)

        # handle when prefix is missing or env var is missing
        if prefix is None:
            prefix = 'openshift/origin'

        self._namespace, self._prefix = prefix.split('/', 2)

    def _image_rebuild_requested(self, image):
        """
        Check if an image should be rebuilt.

        An image rebuild is requested if the user provides the image name
        or image suffix explicitly.
        """

        return (image in self._images) or self.full_name(image) in self._images

    def full_name(self, image):
        """
        Convert a short image name into a full image name.

        The full name of the image will contain the image namespace as well as
        the prefix, if applicable.
        """
        if image in self.UNPREFIXED_IMAGES or image == self._prefix:
            return "{}/{}".format(self._namespace, image)

        return "{}/{}-{}".format(self._namespace, self._prefix, image)

    def config_for(self, image):
        """
        Generate the build config for an image.

        The build config for an image contains instructions for which files
        go into a built image, and where.  Information is taken from
        IMAGE_CONFIGS, and filled out with CONFIG_DEFAULTS.
        """
        base_config = self.IMAGE_CONFIGS.get(image)
        if base_config is None:
            return None

        res = {k: v for (k, v) in base_config.items()}
        for (k, v) in self.CONFIG_DEFAULTS.items():
            res.setdefault(k, v)

        return res

    def requested_images(self):
        """Yield all images requested to be rebuilt, with associate configs."""
        # the base image
        if self._image_rebuild_requested(self._prefix) or self.rebuild_all:
            yield (self._prefix, self.BASE_IMAGE_CONFIG)

        for name in self.IMAGE_CONFIGS:
            if self.image_rebuild_requested(name) or self.rebuild_all:
                yield (name, self.config_for(name))

    def known_images(self):
        """Yield all known images as short names."""
        yield self._prefix
        for name in self.IMAGE_CONFIGS:
            yield name

    @property
    def rebuild_all(self):
        """Whether or not all images should be rebuilt."""
        return len(self._images) == 0


class ImageBuilder(object):
    def __init__(self, image_manager, root_dir):
        self._mgr = image_manager

        self._bin_path = os.path.join(
            root_dir, "_output", "local", "bin", "linux", "amd64")
        self._img_path = os.path.join(root_dir, "images")

        self._context_dir = tempfile.mkdtemp()
        LOG.debug("Created temporary context dir at %s", self._context_dir)
        os.mkdir(os.path.join(self._context_dir, "bin"))
        os.mkdir(os.path.join(self._context_dir, "src"))

    def __enter__(self):
        # we initialize on creation
        pass

    def _cleanup(self):
        """Clean up the generated context directory."""
        if self._context_dir is not None:
            LOG.debug("Cleaning up temporary context dir at %s",
                      self._context_dir)
            shutil.rmtree(self._context_dir)
            self._context_dir = None

    def __exit__(self, exc_type, exc_value, traceback):
        self._cleanup()

    def __del__(self):
        self._cleanup()

    def _add_to_context(self, source, destination, container_destination):
        """
        Add a file to the context directory, and to the Dockerfile.

        Add the given file to the context directory, and then append an ADD
        line to the Dockerfile to place it in the container filesystem at
        the correct destination.
        """
        LOG.debug("Adding file:\n\tfrom %s\n\tto %s"
                  "\n\tincluding in container at %s",
                  source,
                  os.path.join(self._context_dir, destination),
                  container_destination)
        absolute_destination = os.path.abspath(
                os.path.join(self._context_dir, destination))
        if os.path.isdir(source):
            dir_util.copy_tree(source, absolute_destination)
        else:
            shutil.copy(source, absolute_destination)

        out_path = os.path.join(self._context_dir, "Dockerfile")
        with open(out_path, "a") as dockerfile:
            dockerfile.write("ADD {} {}\n".format(
                destination, container_destination))

    def build_image(self, image, config):
        """Build an the given short-named image using the given config."""
        full_name = self._mgr.full_name(image)

        dockerfile_path = os.path.join(self._context_dir, "Dockerfile")
        with open(dockerfile_path, "w+") as dockerfile:
            dockerfile.write("FROM {}\n".format(full_name))

        for binary in config.get("binaries", []):
            self._add_to_context(
                source=os.path.join(self._bin_path, binary),
                destination=os.path.join("bin", binary),
                container_destination=config["binaries"][binary]
            )

        os.mkdir(os.path.join(self._context_dir, "src", image))
        for extra_file in config.get("files", []):
            source_path = os.path.join(
                    self._img_path, config["directory"], extra_file),

            self._add_to_context(
                source=source_path,
                destination=os.path.join("src", image, extra_file),
                container_destination=config["files"][extra_file]
            )

        LOG.debug("Initiating Docker build with Dockerfile:\n%s",
                  open(os.path.join(self._context_dir, "Dockerfile")).read())
        subprocess.call(
                ["docker", "build", "-t", full_name, "."],
                cwd=self._context_dir)

        os.remove(os.path.join(self._context_dir, "Dockerfile"))
        shutil.rmtree(os.path.join(self._context_dir, "src", image))


if __name__ == "__main__":
    parser = argparse.ArgumentParser(
        description="Quickly re-build images depending on "
                    "OpenShift Origin build artifacts.",
        epilog="""
This script re-builds OpenShift Origin images quickly. It is intended
to be used by developers for quick, iterative development of images
that depend on binaries, RPMs, or other artifacts that the Origin build
process generates. The script works by creating a temporary context
directory for a Docker build, adding a simple Dockerfile FROM the image
you wish to rebuild, ADDing in static files to overwrite, and building.

The script supports ADDing binaries from
origin/_output/local/bin/linux/amd64/ and ADDing static files from the
original context directories under the origin/images/ directories.

Specific images can be specified to be built with either the full name
of the image (e.g. openshift3/ose-haproxy-router) or the name sans
prefix (e.g. haproxy-router).

The following environment veriables are honored by this script:
- $OS_IMAGE_PREFIX: one of [openshift/origin, openshift3/ose]
- $OS_DEBUG: if set, debugging information will be printed

Examples:
  # build all images
  build-local-images.py

  # build only the f5-router image
  build-local-images.py f5-router

  # build with a different image prefix
  OS_IMAGE_PREFIX=openshift3/ose build-local-images.sh
    """,
        formatter_class=argparse.RawDescriptionHelpFormatter,
    )

    parser.add_argument("images", metavar="IMAGE", nargs="*",
                        help="images to rebuild (e.g. f5-router), or all if "
                        "no specific images are listed")
    args = parser.parse_args()

    openshift_root = os.path.abspath(
            os.path.join(os.path.dirname(__file__), ".."))

    manager = ImageManager(args.images, os.getenv("OS_IMAGE_PREFIX"))
    builder = ImageBuilder(manager, openshift_root)

    build_occurred = False
    for image, config in manager.requested_images():
        build_occurred = True
        LOG.info("Building %s...", image)
        builder.build_image(image, config)

    if not build_occurred and not manager.rebuild_all:
        LOG.error("The provided image names (%s) "
                  "did not match any buildable images.",
                  ", ".join(args.images))
        LOG.error("This script knows how to build:\n\t%s",
                  "\n\t".join(manager.known_images()))
        sys.exit(1)
