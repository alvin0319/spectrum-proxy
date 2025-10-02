#!/bin/bash

git clone https://github.com/oomph-ac/dragonfly
git clone https://github.com/oomph-ac/oomph

cp patches/0001-fixes-for-latest-gophertunnel.patch oomph/
cp patches/0001-fix-for-latest-gophertunnel-for-df.patch dragonfly/

cd oomph && git am 0001-fixes-for-latest-gophertunnel.patch --whitespace=fix
cd ../dragonfly && git am 0001-fix-for-latest-gophertunnel-for-df.patch

go mod tidy && go get
