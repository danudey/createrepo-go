# Changelog

## 1.0.0 (2026-11-21)


### Features

* add createapt-go and share a core with createrepo-go ([#4](https://github.com/danudey/createrepo-go/issues/4)) ([5d7c0a0](https://github.com/danudey/createrepo-go/commit/5d7c0a0fa1f27562d37ab2ca56944edaf1c224c9))
* add progress reporting ([#2](https://github.com/danudey/createrepo-go/issues/2)) ([75808a2](https://github.com/danudey/createrepo-go/commit/75808a2764f4673ab5baf8370d675314335b25d2))
* add takeover subcommand ([#1](https://github.com/danudey/createrepo-go/issues/1)) ([ed05ada](https://github.com/danudey/createrepo-go/commit/ed05adae57205607a692d9a51f4950d24f09f8ff))
* **createrepo-go:** verify package integrity and signatures ([#7](https://github.com/danudey/createrepo-go/issues/7)) ([7639bb2](https://github.com/danudey/createrepo-go/commit/7639bb2dfb1925d083034fd031353c904901c42a))


### Bug Fixes

* **backend:** let SFTP and HTTP transfers time out or be cancelled ([#28](https://github.com/danudey/createrepo-go/issues/28)) ([9d8cbd8](https://github.com/danudey/createrepo-go/commit/9d8cbd8563f7ccb388c3b8bc204d3cfbec29560c))
* **backend:** never delete the live file when an SFTP upload fails ([#23](https://github.com/danudey/createrepo-go/issues/23)) ([cada31e](https://github.com/danudey/createrepo-go/commit/cada31eba9ef690411240ecb358b6e47efab0389))
* **backend:** publish local files world-readable ([#20](https://github.com/danudey/createrepo-go/issues/20)) ([727c6ba](https://github.com/danudey/createrepo-go/commit/727c6baee3bbc34a78659ca92d837ae4f9137285))
* **copy:** refuse to silently replace a differing destination file ([#26](https://github.com/danudey/createrepo-go/issues/26)) ([da2a636](https://github.com/danudey/createrepo-go/commit/da2a636eff1ca2512bfc49c93d2fc008ca603727))
* **createapt-go:** keep Valid-Until across publishes ([#29](https://github.com/danudey/createrepo-go/issues/29)) ([0d32ef8](https://github.com/danudey/createrepo-go/commit/0d32ef82b9a46706d5f516ad6cb34757955d4654))
* **createapt-go:** parse epochs and revisions as dpkg does ([#34](https://github.com/danudey/createrepo-go/issues/34)) ([e5a8427](https://github.com/danudey/createrepo-go/commit/e5a8427bbbce7421368d14e35e11bba41a745697))
* **createapt-go:** publish a transforming copy into the suite it reports ([#22](https://github.com/danudey/createrepo-go/issues/22)) ([14939d4](https://github.com/danudey/createrepo-go/commit/14939d4b0c608cc89763c6ad3091306028ab4ea7))
* **createrepo-go:** copy a metadata-only signed source without --skip-verify ([#33](https://github.com/danudey/createrepo-go/issues/33)) ([db1f165](https://github.com/danudey/createrepo-go/commit/db1f165669745675da0162975db608ce1415f053))
* **createrepo-go:** keep metadata this tool does not generate ([#21](https://github.com/danudey/createrepo-go/issues/21)) ([c99fd3d](https://github.com/danudey/createrepo-go/commit/c99fd3d03cb51f71ac651bb329fb0bcead7651d5))
* **createrepo-go:** pass the rpmsign passphrase through a private file ([#5](https://github.com/danudey/createrepo-go/issues/5)) ([a60a23e](https://github.com/danudey/createrepo-go/commit/a60a23ed4eaf6839c0364abf2f61ba885e7c3503))
* **createrepo-go:** refuse two packages at one location ([#24](https://github.com/danudey/createrepo-go/issues/24)) ([a136800](https://github.com/danudey/createrepo-go/commit/a1368004fdb04a8ea83beb6ac3e894149989241c))
* **createrepo-go:** use the C-locale summary and description ([#30](https://github.com/danudey/createrepo-go/issues/30)) ([fc207a7](https://github.com/danudey/createrepo-go/commit/fc207a798b203f28bbca0c657acae2d916da2c9a))
* **gpg:** sign InRelease with the signing subkey ([#31](https://github.com/danudey/createrepo-go/issues/31)) ([e2be55b](https://github.com/danudey/createrepo-go/commit/e2be55b72a7d73686d370d8463e33a1efaed23ea))
* **publish:** stop a publish that would undo a concurrent one ([#27](https://github.com/danudey/createrepo-go/issues/27)) ([4e5b936](https://github.com/danudey/createrepo-go/commit/4e5b93610d395755f8e13125cba124f0dc0cca5e))
* **security:** bound APT index downloads and decompression ([#15](https://github.com/danudey/createrepo-go/issues/15)) ([f96b1cb](https://github.com/danudey/createrepo-go/commit/f96b1cb8139170119aceac0118bb00677dbc35ba))
* **security:** check RPM metadata against repomd.xml before using it ([#10](https://github.com/danudey/createrepo-go/issues/10)) ([829069d](https://github.com/danudey/createrepo-go/commit/829069d32689a7f0590c130db5cc6c25197bd420))
* **security:** clean up signing secrets when interrupted ([#17](https://github.com/danudey/createrepo-go/issues/17)) ([f429bed](https://github.com/danudey/createrepo-go/commit/f429bed86fd40e6a39b5c035aa99b2bb8c79eed3))
* **security:** escape every string written into repodata XML ([#9](https://github.com/danudey/createrepo-go/issues/9)) ([1d2af61](https://github.com/danudey/createrepo-go/commit/1d2af6117a8e828d3b77d524646d5df25cc98907))
* **security:** keep passwords in locations out of output and config ([#11](https://github.com/danudey/createrepo-go/issues/11)) ([4cbc175](https://github.com/danudey/createrepo-go/commit/4cbc175e84ad92c1df3a648d92193704f46a5422))
* **security:** only follow http(s) repositories from a fetched config ([#16](https://github.com/danudey/createrepo-go/issues/16)) ([5144e93](https://github.com/danudey/createrepo-go/commit/5144e93a3a74f79ce1ef76eb17c5f477d5016eae))
* **security:** refuse APT package names that would leave the pool ([#14](https://github.com/danudey/createrepo-go/issues/14)) ([9f276b0](https://github.com/danudey/createrepo-go/commit/9f276b02820df26ac5199515c16a4dcf83007839))
* **security:** refuse GPG key ids that gpg or rpm would execute ([#8](https://github.com/danudey/createrepo-go/issues/8)) ([1cfa809](https://github.com/danudey/createrepo-go/commit/1cfa8091a656de78bec318f3e08966cbc5ef7648))
* **security:** stop --help from printing the signing passphrase ([#12](https://github.com/danudey/createrepo-go/issues/12)) ([84436ba](https://github.com/danudey/createrepo-go/commit/84436ba2ccdc8a65ebe4df6ba160fff8b8c79a9a))
* **security:** verify the Release signature in createapt-go verify ([#18](https://github.com/danudey/createrepo-go/issues/18)) ([797975b](https://github.com/danudey/createrepo-go/commit/797975b56cdf2d07351296a39669a7ed1e5fde5d))
* **takeover:** read a --gpg-key file instead of using its path as a key id ([#32](https://github.com/danudey/createrepo-go/issues/32)) ([2c4a4c1](https://github.com/danudey/createrepo-go/commit/2c4a4c1259e5c00014423c34750f8a0f89baf15d))
