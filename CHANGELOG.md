# Changelog

## [0.2.0](https://github.com/sympozium-ai/llmfit-dra/compare/v0.1.0...v0.2.0) (2026-07-02)


### ⚠ BREAKING CHANGES

* stable PCI-address-derived device names (audit blocker #1)

### Features

* DRA ResourceSlice publisher with llmfit capability assessment ([19a3d3d](https://github.com/sympozium-ai/llmfit-dra/commit/19a3d3dee20f1f7e83b4a9b03fb54f12174ef9e8))
* Helm chart install path + release-please versioning ([1794578](https://github.com/sympozium-ai/llmfit-dra/commit/179457837e6dc671036c95015bd09c9ad86c691a))
* honest health — driver binding and RAS errors replace hardcoded true ([f128f2c](https://github.com/sympozium-ai/llmfit-dra/commit/f128f2cea2e10aa2618ddaf8bb9b83e153fe0302))
* kubelet DRA plugin — NodePrepareResources to CDI ([e5101d9](https://github.com/sympozium-ai/llmfit-dra/commit/e5101d9535e1559c2c4fc88dfe061fd54844e8f9))
* ops + security hardening batch (audit P4.5) ([400b0a4](https://github.com/sympozium-ai/llmfit-dra/commit/400b0a43cce82b8dbc277d930ebb403622f12040))
* pin llmfit as a submodule, hermetic image build, GHCR CI ([08aba59](https://github.com/sympozium-ai/llmfit-dra/commit/08aba59ac519b61d6c7932b3087fb087e9471b9e))
* probe captures per-device /dev nodes ([5d8bd10](https://github.com/sympozium-ai/llmfit-dra/commit/5d8bd109bb981862f8089cb2836a0940978d5b35))
* ship DeviceClasses for llmfit.ai devices ([e50a80d](https://github.com/sympozium-ai/llmfit-dra/commit/e50a80d15f89ed533e776e74cd70e10a5b33d72f))
* stable PCI-address-derived device names (audit blocker [#1](https://github.com/sympozium-ai/llmfit-dra/issues/1)) ([6a81dae](https://github.com/sympozium-ai/llmfit-dra/commit/6a81daef1b7bb0f3377c637210987e76ef846403))
* standardized pcieRoot attribute + matchAttribute alignment ([a1cb5bd](https://github.com/sympozium-ai/llmfit-dra/commit/a1cb5bde9a4826f5e8d151369e64011575283fc8))
* uevent hot-attach + gated device taints — Phase 3 liveness ([5129078](https://github.com/sympozium-ai/llmfit-dra/commit/5129078bb21b11f7c07fd6465c8f1b936f7ad84e))
* vendor coexistence — vendor DRA presence demotes GPUs to fitness-only ([a90ebdd](https://github.com/sympozium-ai/llmfit-dra/commit/a90ebddfc2fb9dfac11da56e9e291e41d05e4f00))


### Bug Fixes

* EXIT trap valid on every path; CI installs the admission policy ([27080a5](https://github.com/sympozium-ai/llmfit-dra/commit/27080a5f022e1d9ae6ac7dbda80ea60223ab0073))
* publish capacity and unifiedMemory only when a source knows them ([7024e3c](https://github.com/sympozium-ai/llmfit-dra/commit/7024e3c2434463cbe3b1bd31c96a9800ff92ceca))
* virtual display adapters must not count as fit-capable GPUs ([b001297](https://github.com/sympozium-ai/llmfit-dra/commit/b0012978f05c248a8bd763a705c9ce865b9b0d9c))
