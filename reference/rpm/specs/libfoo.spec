Name:           libfoo
Version:        1.3.0
Release:        1
Summary:        Foo shared library
License:        MIT
URL:            https://example.com/foo
BuildArch:      x86_64
Requires:       glibc
Provides:       libfoo.so.1()(64bit)

%description
Foo library, second test package with a different arch.

%install
mkdir -p %{buildroot}%{_libdir}
echo "binary" > %{buildroot}%{_libdir}/libfoo.so.1.3.0
ln -s libfoo.so.1.3.0 %{buildroot}%{_libdir}/libfoo.so.1

%files
%{_libdir}/libfoo.so.1.3.0
%{_libdir}/libfoo.so.1

%changelog
* Mon Jun 15 2026 Tigera Test <test@tigera.io> - 1.3.0-1
- Initial package
