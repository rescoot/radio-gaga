#!/bin/bash
set -e
U=${1:-https://sunshine.rescoot.org/radio-gaga/radio-gaga-arm}
P=/var/rootdirs/home/root/radio-gaga/radio-gaga
N=$P.new
R=$(curl -sI $U|grep -i last-modified|cut -d' ' -f2-|sed 's/GMT/+0000/'|tr -d '\r\n')
[ -f $P ]&&{ L=$(date -u -r $P --rfc-2822);[ "$R" = "$L" ]&&{ echo Binary up to date;exit 0;};echo "L: $L";};echo "R: $R"
curl -fsSL -C - -o $N $U
chmod 755 $N
touch -d "$R" $N
mv -f $N $P
systemctl restart rescoot-radio-gaga
echo Done