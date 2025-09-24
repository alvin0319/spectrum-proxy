#!/bin/bash

git clone https://github.com/oomph-ac/dragonfly
git clone https://github.com/oomph-ac/oomph

cp patches/* oomph/

cd oomph && git am 0001-yay.patch --whitespace=fix

go mod tidy && go get
