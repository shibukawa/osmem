"""Platform-specific wheel support.

The wheel contains the osmem-server binary for one platform, so it must not
be tagged as pure Python. scripts/build-python-wheels.sh copies the binary
into osmem/bin/ and sets OSMEM_PLAT_NAME before building.
"""

import os

from setuptools import Distribution, setup

try:
    from wheel.bdist_wheel import bdist_wheel as _bdist_wheel
except ImportError:  # setuptools >= 70 vendors it
    from setuptools.command.bdist_wheel import bdist_wheel as _bdist_wheel


class BinaryDistribution(Distribution):
    def has_ext_modules(self):
        return os.path.isdir(os.path.join(os.path.dirname(__file__), "osmem", "bin"))


class bdist_wheel(_bdist_wheel):
    def finalize_options(self):
        plat = os.environ.get("OSMEM_PLAT_NAME")
        if plat:
            self.plat_name = plat
            self.plat_name_supplied = True
        super().finalize_options()
        self.root_is_pure = False

    def get_tag(self):
        _, _, plat = super().get_tag()
        return "py3", "none", plat


setup(distclass=BinaryDistribution, cmdclass={"bdist_wheel": bdist_wheel})
