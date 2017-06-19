#!/usr/bin/env python2

from shutil import copy, rmtree
import distutils.dir_util as dir_util
from subprocess import call
from tempfile import mkdtemp

from atexit import register
from os import getenv, mkdir, remove
from os.path import abspath, dirname, isdir, join

import argparse

PARSER = argparse.ArgumentParser(
    description="Quickly re-build images depending on OpenShift Origin build artifacts.",
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

PARSER.add_argument("images", metavar="IMAGE", nargs="*",
                    help="images to rebuild (e.g. f5-router), or all if no "
                    "specific images are listed")
ARGS = PARSER.parse_args()

OS_IMAGE_PREFIX = getenv("OS_IMAGE_PREFIX", "openshift/origin")
IMAGE_NAMESPACE, IMAGE_PREFIX = OS_IMAGE_PREFIX.split("/", 2)

IMAGE_CONFIG = {
    IMAGE_PREFIX: {
        "directory": "origin",
        "binaries": {
            "openshift": "/usr/bin/openshift"
        },
        "files": {}
    },
    "deployer": {
        "directory": "deployer",
        "binaries": {
            "openshift": "/usr/bin/openshift"
        },
        "files": {}
    },
    "recycler": {
        "directory": "recycler",
        "binaries": {
            "openshift": "/usr/bin/openshift"
        },
        "files": {}
    },
    "docker-builder": {
        "directory": "builder/docker/docker-builder",
        "binaries": {
            "openshift": "/usr/bin/openshift"
        },
        "files": {}
    },
    "sti-builder": {
        "directory": "builder/docker/sti-builder",
        "binaries": {
            "openshift": "/usr/bin/openshift"
        },
        "files": {}
    },
    "f5-router": {
        "directory": "router/f5",
        "binaries": {
            "openshift": "/usr/bin/openshift"
        },
        "files": {}
    },
    "haproxy-router": {
        "directory": "router/haproxy",
        "binaries": {
            "openshift": "/usr/bin/openshift"
        },
        "files": {
            ".": "/var/lib/haproxy"
        }
    },
    "keepalived-ipfailover": {
        "directory": "ipfailover/keepalived",
        "binaries": {
            "openshift": "/usr/bin/openshift"
        },
        "files": {
            ".": "/var/lib/ipfailover/keepalived"
        }
    },
    "node": {
        "directory": "node",
        "binaries": {
            "openshift": "/usr/bin/openshift"
        },
        "files": {}
    },
    "openvswitch": {
        "directory": "openvswitch",
        "binaries": {
            "openshift": "/usr/bin/openshift"
        },
        "files": {}
    }
}


def image_rebuild_requested(image):
    """
    An image rebuild is requested if the
    user provides the image name or image
    suffix explicitly or does not provide
    any explicit requests.
    """

    if len(ARGS.images) == 0:
        return False

    if len(ARGS.images) == 1:
        return True

    return (image in ARGS.images) or full_name(image) in ARGS.images


def full_name(image):
    """
    The full name of the image will contain
    the image namespace as well as the pre-
    fix, if applicable.
    """
    if image in ["node", "openvswitch", IMAGE_PREFIX]:
        return "{}/{}".format(IMAGE_NAMESPACE, image)

    return "{}/{}-{}".format(IMAGE_NAMESPACE, IMAGE_PREFIX, image)


def add_to_context(CONTEXT_DIR, source, destination, container_destination):
    """
    Add a file to the context directory
    and add an entry to the Dockerfile
    to place it in the container file-
    sytem at the correct destination.
    """
    debug(
        "Adding file:\n\tfrom {}\n\tto {}"
        "\n\tincluding in container at {}".format(
          source,
          join(CONTEXT_DIR, destination),
          container_destination))
    absolute_destination = abspath(join(CONTEXT_DIR, destination))
    if isdir(source):
        dir_util.copy_tree(source, absolute_destination)
    else:
        copy(source, absolute_destination)
    with open(join(CONTEXT_DIR, "Dockerfile"), "a") as dockerfile:
        dockerfile.write("ADD {} {}\n".format(
            destination, container_destination))


def debug(message):
    if getenv("OS_DEBUG"):
        print "[DEBUG] {}".format(message)


OS_ROOT = abspath(join(dirname(__file__), ".."))
OS_BIN_PATH = join(OS_ROOT, "_output", "local", "bin", "linux", "amd64")
OS_IMAGE_PATH = join(OS_ROOT, "images")

CONTEXT_DIR = mkdtemp()
register(rmtree, CONTEXT_DIR)

debug("Created temporary context dir at {}".format(CONTEXT_DIR))
mkdir(join(CONTEXT_DIR, "bin"))
mkdir(join(CONTEXT_DIR, "src"))

build_occurred = False
for image in IMAGE_CONFIG:
    if not image_rebuild_requested(image):
        continue

    build_occurred = True
    print "[INFO] Building {}...".format(image)
    with open(join(CONTEXT_DIR, "Dockerfile"), "w+") as dockerfile:
        dockerfile.write("FROM {}\n".format(full_name(image)))

    config = IMAGE_CONFIG[image]
    for binary in config.get("binaries", []):
        add_to_context(
            CONTEXT_DIR,
            source=join(OS_BIN_PATH, binary),
            destination=join("bin", binary),
            container_destination=config["binaries"][binary]
        )

    mkdir(join(CONTEXT_DIR, "src", image))
    for file in config.get("files", []):
        add_to_context(
            CONTEXT_DIR,
            source=join(OS_IMAGE_PATH, config["directory"], file),
            destination=join("src", image, file),
            container_destination=config["files"][file]
        )

    debug("Initiating Docker build with Dockerfile"
          ":\n{}".format(open(join(CONTEXT_DIR, "Dockerfile")).read()))
    call(["docker", "build", "-t", full_name(image), "."], cwd=CONTEXT_DIR)

    remove(join(CONTEXT_DIR, "Dockerfile"))
    rmtree(join(CONTEXT_DIR, "src", image))

if not build_occurred and len(ARGS.images) > 1:
    print ("[ERROR] The provided image names "
           "({}) did not match any buildable images.".format(
            ", ".join(ARGS.images)))
    print "[ERROR] This script knows how to build:\n\t{}".format(
        "\n\t".join(map(full_name, IMAGE_CONFIG.keys()))
    )
    exit(1)
