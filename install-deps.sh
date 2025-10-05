#!/bin/bash

git clone https://github.com/oomph-ac/dragonfly
git clone https://github.com/oomph-ac/oomph

cp patches/0001-Fixes.patch oomph/

cd oomph && git am 0001-Fixes.patch

go mod tidy && go get
