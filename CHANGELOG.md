# Changelog

## [0.2.7](https://github.com/sympozium-ai/llmfit-dra/compare/v0.2.6...v0.2.7) (2026-07-03)


### Features

* **modelclaim:** allocation-aware Satisfiable — available count and holder ([6e6bc17](https://github.com/sympozium-ai/llmfit-dra/commit/6e6bc17cc2b6d502a3d26dccd38b7be63fc842fd))
* **modelclaim:** allocation-aware Satisfiable (closes [#21](https://github.com/sympozium-ai/llmfit-dra/issues/21)) ([8b2da49](https://github.com/sympozium-ai/llmfit-dra/commit/8b2da490275eafeb141e308ebabd0c1ceb64f43a))


### Bug Fixes

* **release:** draft releases + docs: sympozium integration design ([e8dcabf](https://github.com/sympozium-ai/llmfit-dra/commit/e8dcabf99036bb3d1ecde4434e9fb8208f6990cf))
* **release:** prerelease-not-draft — drafts create no tag and loop release-please ([36345e9](https://github.com/sympozium-ai/llmfit-dra/commit/36345e916a66096bca7ee8c667747df4c481f4db))
* **release:** prerelease-not-draft — stop the release-please loop ([b742dd8](https://github.com/sympozium-ai/llmfit-dra/commit/b742dd8b5cb928b6a91bd6fce472b5fda5757f71))
* **release:** publish releases only after image and chart exist ([933acb9](https://github.com/sympozium-ai/llmfit-dra/commit/933acb95c8e5061e1e2a16a9aa8de6c71a0c8d8c))

## [0.2.6](https://github.com/sympozium-ai/llmfit-dra/compare/v0.2.5...v0.2.6) (2026-07-03)


### Features

* ModelClaim CRD + controller — request hardware by model capability ([67b73c7](https://github.com/sympozium-ai/llmfit-dra/commit/67b73c7b928fc9d2451eb4bc3644861958cc6d3a))
* ModelClaim CRD + controller — request hardware by model capability (M1) ([9d71fe1](https://github.com/sympozium-ai/llmfit-dra/commit/9d71fe10e1a2616aa12d3d71cb02570b81f2be60))


### Bug Fixes

* **chart:** use explicit selector labels; design doc status -&gt; accepted ([8abfd9b](https://github.com/sympozium-ai/llmfit-dra/commit/8abfd9b2a87cd0a8eff3faffc63eb5cedfad2962))
* controller pods must not match the DaemonSet's selector labels ([67760e9](https://github.com/sympozium-ai/llmfit-dra/commit/67760e9a3c9d49972d2220551749ed3966dfd8f4))
* **e2e:** Scenario 5 API-transport window is racy on fresh deploys ([44bb160](https://github.com/sympozium-ai/llmfit-dra/commit/44bb160130a4b1ae27533f0467646104fce03232))
* **e2e:** widen Scenario 5 API-transport window 60s-&gt;180s (flaky on fresh deploys) ([1b1fbb7](https://github.com/sympozium-ai/llmfit-dra/commit/1b1fbb7839fd4cdf4a4916dcba480ec8deccc0a6))

## [0.2.5](https://github.com/sympozium-ai/llmfit-dra/compare/v0.2.4...v0.2.5) (2026-07-03)


### Bug Fixes

* **e2e:** widen Scenario 8 restart timeout (flaky, not a regression) ([161582b](https://github.com/sympozium-ai/llmfit-dra/commit/161582bd3637492465bdcbae8e64007a4efc7239))
* **e2e:** widen Scenario 8 restart timeout 120s→300s (flaky) ([febfec8](https://github.com/sympozium-ai/llmfit-dra/commit/febfec80ec07dbd2352a9f3ea6446367125376d5))

## [0.2.4](https://github.com/sympozium-ai/llmfit-dra/compare/v0.2.3...v0.2.4) (2026-07-03)


### Bug Fixes

* **ci:** chart publish — docker login before manifest inspect ([34115f1](https://github.com/sympozium-ai/llmfit-dra/commit/34115f1e84eddbff099e6b4c37b34c3613aeea0f))
* **ci:** docker login before manifest inspect in the chart job ([86a76f7](https://github.com/sympozium-ai/llmfit-dra/commit/86a76f75ab6433b084833de5b67bc16d185ff38d))

## [0.2.3](https://github.com/sympozium-ai/llmfit-dra/compare/v0.2.2...v0.2.3) (2026-07-03)


### Bug Fixes

* bump llmfit submodule to v0.9.36 — multi-GPU and Intel iGPU detection fixes ([d9a220f](https://github.com/sympozium-ai/llmfit-dra/commit/d9a220f4fa5872c1ab0085766580367756f8a69c))
* bump llmfit submodule to v0.9.36 (multi-GPU + Intel detection fixes) ([1d1f904](https://github.com/sympozium-ai/llmfit-dra/commit/1d1f90442db4d7081571f909ff490d42cc91d8fa))
* **ci:** move RELEASE_TOKEN guard into the shell — secrets is not a valid if: context ([a43e7b3](https://github.com/sympozium-ai/llmfit-dra/commit/a43e7b32bd41aae29ecff6ea09ae4678e63b7d66))
* **ci:** RELEASE_TOKEN guard — secrets is not a valid if: context ([5ce13f9](https://github.com/sympozium-ai/llmfit-dra/commit/5ce13f9af893c0cff075c22918fc3752f72219ed))

## [0.2.2](https://github.com/sympozium-ai/llmfit-dra/compare/v0.2.1...v0.2.2) (2026-07-02)


### Bug Fixes

* verify GHCR token has read:packages before writing pull-secret ([05dc79f](https://github.com/sympozium-ai/llmfit-dra/commit/05dc79fbd71e53c0632347d9e738f5eb21073235))

## [0.2.1](https://github.com/sympozium-ai/llmfit-dra/compare/v0.2.0...v0.2.1) (2026-07-02)


### Features

* consume llmfit over its serve API on an AF_UNIX socket ([4f3e758](https://github.com/sympozium-ai/llmfit-dra/commit/4f3e7582b4452ba043d01fd263739fd24bffb9b7))

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
