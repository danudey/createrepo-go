# Changelog

## 1.0.0 (2026-10-30)


### Features

* add createapt-go and share a core with createrepo-go ([#4](https://github.com/danudey/createrepo-go/issues/4)) ([5d7c0a0](https://github.com/danudey/createrepo-go/commit/5d7c0a0fa1f27562d37ab2ca56944edaf1c224c9))
* add progress reporting ([#2](https://github.com/danudey/createrepo-go/issues/2)) ([75808a2](https://github.com/danudey/createrepo-go/commit/75808a2764f4673ab5baf8370d675314335b25d2))
* add takeover subcommand ([#1](https://github.com/danudey/createrepo-go/issues/1)) ([ed05ada](https://github.com/danudey/createrepo-go/commit/ed05adae57205607a692d9a51f4950d24f09f8ff))
* **createrepo-go:** verify package integrity and signatures ([#7](https://github.com/danudey/createrepo-go/issues/7)) ([7639bb2](https://github.com/danudey/createrepo-go/commit/7639bb2dfb1925d083034fd031353c904901c42a))


### Bug Fixes

* **createrepo-go:** pass the rpmsign passphrase through a private file ([#5](https://github.com/danudey/createrepo-go/issues/5)) ([a60a23e](https://github.com/danudey/createrepo-go/commit/a60a23ed4eaf6839c0364abf2f61ba885e7c3503))
* **security:** check RPM metadata against repomd.xml before using it ([#10](https://github.com/danudey/createrepo-go/issues/10)) ([829069d](https://github.com/danudey/createrepo-go/commit/829069d32689a7f0590c130db5cc6c25197bd420))
* **security:** escape every string written into repodata XML ([#9](https://github.com/danudey/createrepo-go/issues/9)) ([1d2af61](https://github.com/danudey/createrepo-go/commit/1d2af6117a8e828d3b77d524646d5df25cc98907))
* **security:** keep passwords in locations out of output and config ([#11](https://github.com/danudey/createrepo-go/issues/11)) ([4cbc175](https://github.com/danudey/createrepo-go/commit/4cbc175e84ad92c1df3a648d92193704f46a5422))
* **security:** refuse GPG key ids that gpg or rpm would execute ([#8](https://github.com/danudey/createrepo-go/issues/8)) ([1cfa809](https://github.com/danudey/createrepo-go/commit/1cfa8091a656de78bec318f3e08966cbc5ef7648))
* **security:** stop --help from printing the signing passphrase ([#12](https://github.com/danudey/createrepo-go/issues/12)) ([84436ba](https://github.com/danudey/createrepo-go/commit/84436ba2ccdc8a65ebe4df6ba160fff8b8c79a9a))
