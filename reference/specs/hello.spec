Name:           hello
Version:        2.10
Release:        3%{?dist}
Summary:        A friendly greeting program
License:        GPLv3+
URL:            https://www.gnu.org/software/hello/
Group:          Applications/Text
Vendor:         Tigera Test
BuildArch:      noarch
Requires:       bash >= 4.0
Requires:       coreutils
Provides:       greeting = %{version}-%{release}
Conflicts:      goodbye
Obsoletes:      hello-old < 2.0

%description
The GNU hello program produces a familiar, friendly greeting.
It allows non-programmers to use a classic computer science tool.

%prep
%build

%install
mkdir -p %{buildroot}%{_bindir}
cat > %{buildroot}%{_bindir}/hello <<'SH'
#!/bin/sh
echo "Hello, world!"
SH
chmod 0755 %{buildroot}%{_bindir}/hello
mkdir -p %{buildroot}%{_datadir}/doc/hello
echo "doc" > %{buildroot}%{_datadir}/doc/hello/README

%files
%{_bindir}/hello
%dir %{_datadir}/doc/hello
%{_datadir}/doc/hello/README

%changelog
* Mon Jun 01 2026 Tigera Test <test@tigera.io> - 2.10-3
- Third build for testing
* Sun May 01 2026 Tigera Test <test@tigera.io> - 2.10-2
- Second build
