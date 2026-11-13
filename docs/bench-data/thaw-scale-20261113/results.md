# Thaw at scale — 20261113-022704

NS="1 10 50" MS="1 10 50 200" MODES="frozen warm" REPS="norep rep" R=3 CALLS=5 TIMEOUT=60s
IDLE=10s SETTLE=120s MAXREP=16 PSI_MAX=5 MEM_NOREP=1024M MEM_REP=256M

| cell | N | M | ok | TTFR p50 | p95 | p99 | max | steady p50 | p99 | live peak | machines | PSS peak MiB | min avail MiB | PSI max | t→0 s | status |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| norep-frozen-n1-m1-r1 | 1 | 1 | 1/1 | 526 | 526 | 526 | 526 | 1.7 | 3.1 | 1 | 1 | 17 | 9925 | 0.0 | 14.0 | OK |
| norep-frozen-n1-m1-r2 | 1 | 1 | 1/1 | 565 | 565 | 565 | 565 | 1.7 | 2.1 | 1 | 1 | 17 | 9922 | 0.0 | 13.7 | OK |
| norep-frozen-n1-m1-r3 | 1 | 1 | 1/1 | 594 | 594 | 594 | 594 | 2.2 | 3.3 | 1 | 1 | 18 | 9923 | 0.0 | 13.8 | OK |
| norep-warm-n1-m1-r1 | 1 | 1 | 1/1 | 76 | 76 | 76 | 76 | 1.6 | 1.9 | 1 | 1 | 21 | 9932 | 0.0 | n/a | OK |
| norep-warm-n1-m1-r2 | 1 | 1 | 1/1 | 70 | 70 | 70 | 70 | 1.7 | 2.2 | 1 | 1 | 20 | 9936 | 0.0 | n/a | OK |
| norep-warm-n1-m1-r3 | 1 | 1 | 1/1 | 17 | 17 | 17 | 17 | 1.6 | 1.7 | 1 | 1 | 26 | 9928 | 0.0 | n/a | OK |
| norep-frozen-n1-m10-r1 | 1 | 10 | 10/10 | 985 | 1000 | 1000 | 1000 | 17.5 | 92.4 | 1 | 1 | 35 | 9915 | 0.0 | 14.0 | OK |
| norep-frozen-n1-m10-r2 | 1 | 10 | 10/10 | 606 | 800 | 800 | 800 | 7.1 | 98.4 | 1 | 1 | 31 | 9915 | 0.0 | 15.1 | OK |
| norep-frozen-n1-m10-r3 | 1 | 10 | 10/10 | 838 | 932 | 932 | 932 | 10.1 | 81.8 | 1 | 1 | 30 | 9914 | 0.0 | 13.6 | OK |
| norep-warm-n1-m10-r1 | 1 | 10 | 10/10 | 276 | 387 | 387 | 387 | 5.3 | 76.6 | 1 | 1 | 46 | 9906 | 0.0 | n/a | OK |
| norep-warm-n1-m10-r2 | 1 | 10 | 10/10 | 357 | 460 | 460 | 460 | 4.3 | 71.9 | 1 | 1 | 45 | 9910 | 0.0 | n/a | OK |
| norep-warm-n1-m10-r3 | 1 | 10 | 10/10 | 287 | 411 | 411 | 411 | 3.9 | 93.3 | 1 | 1 | 45 | 9903 | 0.0 | n/a | OK |
| norep-frozen-n1-m50-r1 | 1 | 50 | 27/50 | 1473 | 2643 | 2704 | 2704 | 8.1 | 86.7 | 16 | 16 | 350 | 9714 | 0.0 | 36.7 | DEGRADED |
| norep-frozen-n1-m50-r2 | 1 | 50 | 27/50 | 1491 | 2967 | 3069 | 3069 | 14.3 | 118.7 | 16 | 16 | 415 | 9642 | 0.0 | 37.1 | DEGRADED |
| norep-frozen-n1-m50-r3 | 1 | 50 | 27/50 | 1466 | 2800 | 2956 | 2956 | 8.7 | 291.3 | 16 | 16 | 407 | 9646 | 0.0 | 37.3 | DEGRADED |
| norep-warm-n1-m50-r1 | 1 | 50 | 47/50 | 488 | 588 | 601 | 601 | 29.3 | 125.8 | 16 | 16 | 468 | 9596 | 0.0 | n/a | DEGRADED |
| norep-warm-n1-m50-r2 | 1 | 50 | 47/50 | 449 | 587 | 598 | 598 | 21.4 | 156.8 | 16 | 16 | 459 | 9602 | 0.0 | n/a | DEGRADED |
| norep-warm-n1-m50-r3 | 1 | 50 | 46/50 | 418 | 550 | 553 | 553 | 16.6 | 103.7 | 16 | 16 | 467 | 9593 | 0.0 | n/a | DEGRADED |
| norep-frozen-n1-m200 | 1 | 200 | skipped | | | | | | | | 16 (est) | | 9600 | | | skipped: estimated 16 machine(s) × 1024 MiB = 16384 MiB; available 9600 MiB (70% = 6720), disk free 596727 MiB |
| norep-warm-n1-m200 | 1 | 200 | skipped | | | | | | | | 16 (est) | | 9600 | | | skipped: estimated 16 machine(s) × 1024 MiB = 16384 MiB; available 9600 MiB (70% = 6720), disk free 596727 MiB |
| norep-frozen-n10-m1-r1 | 10 | 1 | 1/1 | 627 | 627 | 627 | 627 | 2.4 | 2.8 | 1 | 1 | 18 | 9912 | 0.0 | 13.6 | OK |
| norep-frozen-n10-m1-r2 | 10 | 1 | 1/1 | 526 | 526 | 526 | 526 | 2.1 | 3.7 | 1 | 1 | 18 | 9907 | 0.0 | 13.7 | OK |
| norep-frozen-n10-m1-r3 | 10 | 1 | 1/1 | 619 | 619 | 619 | 619 | 1.8 | 3.2 | 1 | 1 | 18 | 9908 | 0.0 | 13.9 | OK |
| norep-warm-n10-m1-r1 | 10 | 1 | 1/1 | 104 | 104 | 104 | 104 | 3.7 | 68.9 | 1 | 1 | 26 | 9915 | 0.0 | n/a | OK |
| norep-warm-n10-m1-r2 | 10 | 1 | 1/1 | 80 | 80 | 80 | 80 | 2.4 | 2.9 | 1 | 1 | 20 | 9924 | 0.0 | n/a | OK |
| norep-warm-n10-m1-r3 | 10 | 1 | 1/1 | 77 | 77 | 77 | 77 | 1.8 | 2.6 | 1 | 1 | 21 | 9920 | 0.0 | n/a | OK |
| norep-frozen-n10-m10 | 10 | 10 | skipped | | | | | | | | 10 (est) | | 9921 | | | skipped: estimated 10 machine(s) × 1024 MiB = 10240 MiB; available 9921 MiB (70% = 6944), disk free 596731 MiB |
| norep-warm-n10-m10 | 10 | 10 | skipped | | | | | | | | 10 (est) | | 9921 | | | skipped: estimated 10 machine(s) × 1024 MiB = 10240 MiB; available 9921 MiB (70% = 6944), disk free 596731 MiB |
| norep-frozen-n10-m50 | 10 | 50 | skipped | | | | | | | | 10 (est) | | 9922 | | | skipped: estimated 10 machine(s) × 1024 MiB = 10240 MiB; available 9922 MiB (70% = 6945), disk free 596731 MiB |
| norep-warm-n10-m50 | 10 | 50 | skipped | | | | | | | | 10 (est) | | 9922 | | | skipped: estimated 10 machine(s) × 1024 MiB = 10240 MiB; available 9922 MiB (70% = 6945), disk free 596731 MiB |
| norep-frozen-n10-m200 | 10 | 200 | skipped | | | | | | | | 20 (est) | | 9922 | | | skipped: estimated 20 machine(s) × 1024 MiB = 20480 MiB; available 9922 MiB (70% = 6945), disk free 596731 MiB |
| norep-warm-n10-m200 | 10 | 200 | skipped | | | | | | | | 20 (est) | | 9922 | | | skipped: estimated 20 machine(s) × 1024 MiB = 20480 MiB; available 9922 MiB (70% = 6945), disk free 596731 MiB |
| norep-frozen-n50-m1-r1 | 50 | 1 | 1/1 | 690 | 690 | 690 | 690 | 1.6 | 1.8 | 1 | 1 | 18 | 9909 | 0.0 | 12.0 | OK |
| norep-frozen-n50-m1-r2 | 50 | 1 | 1/1 | 333 | 333 | 333 | 333 | 1.8 | 2.8 | 1 | 1 | 18 | 9912 | 0.0 | 12.4 | OK |
| norep-frozen-n50-m1-r3 | 50 | 1 | 1/1 | 682 | 682 | 682 | 682 | 1.7 | 2.4 | 1 | 1 | 18 | 9912 | 0.0 | 12.6 | OK |
| norep-warm-n50-m1-r1 | 50 | 1 | 1/1 | 70 | 70 | 70 | 70 | 1.6 | 2.6 | 1 | 1 | 21 | 9923 | 0.0 | n/a | OK |
| norep-warm-n50-m1-r2 | 50 | 1 | 1/1 | 26 | 26 | 26 | 26 | 1.6 | 52.7 | 1 | 1 | 27 | 9920 | 0.0 | n/a | OK |
| norep-warm-n50-m1-r3 | 50 | 1 | 1/1 | 83 | 83 | 83 | 83 | 1.7 | 2.6 | 1 | 1 | 21 | 9924 | 0.0 | n/a | OK |
| norep-frozen-n50-m10 | 50 | 10 | skipped | | | | | | | | 10 (est) | | 9926 | | | skipped: estimated 10 machine(s) × 1024 MiB = 10240 MiB; available 9926 MiB (70% = 6948), disk free 596731 MiB |
| norep-warm-n50-m10 | 50 | 10 | skipped | | | | | | | | 10 (est) | | 9926 | | | skipped: estimated 10 machine(s) × 1024 MiB = 10240 MiB; available 9926 MiB (70% = 6948), disk free 596731 MiB |
| norep-frozen-n50-m50 | 50 | 50 | skipped | | | | | | | | 50 (est) | | 9926 | | | skipped: estimated 50 machine(s) × 1024 MiB = 51200 MiB; available 9926 MiB (70% = 6948), disk free 596731 MiB |
| norep-warm-n50-m50 | 50 | 50 | skipped | | | | | | | | 50 (est) | | 9926 | | | skipped: estimated 50 machine(s) × 1024 MiB = 51200 MiB; available 9926 MiB (70% = 6948), disk free 596731 MiB |
| norep-frozen-n50-m200 | 50 | 200 | skipped | | | | | | | | 50 (est) | | 9925 | | | skipped: estimated 50 machine(s) × 1024 MiB = 51200 MiB; available 9925 MiB (70% = 6947), disk free 596731 MiB |
| norep-warm-n50-m200 | 50 | 200 | skipped | | | | | | | | 50 (est) | | 9925 | | | skipped: estimated 50 machine(s) × 1024 MiB = 51200 MiB; available 9925 MiB (70% = 6947), disk free 596731 MiB |
| rep-frozen-n1-m1-r1 | 1 | 1 | 1/1 | 531 | 531 | 531 | 531 | 1.5 | 1.7 | 1 | 1 | 13 | 9927 | 0.0 | 12.0 | OK |
| rep-frozen-n1-m1-r2 | 1 | 1 | 1/1 | 579 | 579 | 579 | 579 | 1.6 | 1.7 | 1 | 1 | 14 | 9927 | 0.0 | 12.2 | OK |
| rep-frozen-n1-m1-r3 | 1 | 1 | 1/1 | 562 | 562 | 562 | 562 | 1.4 | 1.6 | 1 | 1 | 14 | 9925 | 0.0 | 13.7 | OK |
| rep-warm-n1-m1-r1 | 1 | 1 | 1/1 | 11 | 11 | 11 | 11 | 1.4 | 1.6 | 1 | 1 | 15 | 9930 | 0.0 | n/a | OK |
| rep-warm-n1-m1-r2 | 1 | 1 | 1/1 | 11 | 11 | 11 | 11 | 1.5 | 1.5 | 1 | 1 | 15 | 9931 | 0.0 | n/a | OK |
| rep-warm-n1-m1-r3 | 1 | 1 | 1/1 | 15 | 15 | 15 | 15 | 1.4 | 1.7 | 1 | 1 | 15 | 9930 | 0.0 | n/a | OK |
| rep-frozen-n1-m10-r1 | 1 | 10 | 10/10 | 830 | 1241 | 1241 | 1241 | 2.3 | 47.4 | 10 | 10 | 131 | 9874 | 0.0 | 27.2 | OK |
| rep-frozen-n1-m10-r2 | 1 | 10 | 10/10 | 1010 | 1428 | 1428 | 1428 | 2.5 | 42.5 | 10 | 10 | 145 | 9860 | 0.0 | 31.1 | OK |
| rep-frozen-n1-m10-r3 | 1 | 10 | 10/10 | 994 | 1448 | 1448 | 1448 | 2.4 | 42.5 | 10 | 10 | 142 | 9856 | 0.0 | 29.0 | OK |
| rep-warm-n1-m10-r1 | 1 | 10 | 10/10 | 64 | 81 | 81 | 81 | 7.4 | 21.6 | 10 | 10 | 149 | 9858 | 0.0 | n/a | OK |
| rep-warm-n1-m10-r2 | 1 | 10 | 10/10 | 62 | 83 | 83 | 83 | 8.4 | 28.3 | 10 | 10 | 149 | 9864 | 0.0 | n/a | OK |
| rep-warm-n1-m10-r3 | 1 | 10 | 10/10 | 65 | 77 | 77 | 77 | 9.7 | 18.6 | 10 | 10 | 149 | 9857 | 0.0 | n/a | OK |
| rep-frozen-n1-m50-r1 | 1 | 50 | 16/50 | 1573 | 2178 | 2178 | 2178 | 2.2 | 51.1 | 16 | 16 | 227 | 9812 | 0.0 | 38.8 | DEGRADED |
| rep-frozen-n1-m50-r2 | 1 | 50 | 16/50 | 1433 | 2021 | 2021 | 2021 | 2.4 | 50.1 | 16 | 16 | 230 | 9830 | 0.0 | 40.0 | DEGRADED |
| rep-frozen-n1-m50-r3 | 1 | 50 | 16/50 | 1440 | 2137 | 2137 | 2137 | 2.2 | 38.7 | 16 | 16 | 230 | 9830 | 0.0 | 37.1 | DEGRADED |
| rep-warm-n1-m50-r1 | 1 | 50 | 16/50 | 143 | 177 | 177 | 177 | 14.3 | 51.0 | 16 | 16 | 241 | 9826 | 0.0 | n/a | DEGRADED |
| rep-warm-n1-m50-r2 | 1 | 50 | 16/50 | 116 | 133 | 133 | 133 | 14.2 | 28.0 | 16 | 16 | 242 | 9828 | 0.0 | n/a | DEGRADED |
| rep-warm-n1-m50-r3 | 1 | 50 | 16/50 | 123 | 142 | 142 | 142 | 14.2 | 37.8 | 16 | 16 | 243 | 9805 | 0.0 | n/a | DEGRADED |
| rep-frozen-n1-m200-r1 | 1 | 200 | 16/200 | 1679 | 2280 | 2280 | 2280 | 2.2 | 43.3 | 16 | 16 | 236 | 9811 | 0.0 | 36.2 | DEGRADED |
| rep-frozen-n1-m200-r2 | 1 | 200 | 16/200 | 1752 | 2340 | 2340 | 2340 | 2.1 | 18.6 | 16 | 16 | 237 | 9789 | 0.0 | 41.6 | DEGRADED |
| rep-frozen-n1-m200-r3 | 1 | 200 | 16/200 | 1570 | 2163 | 2163 | 2163 | 2.2 | 24.2 | 16 | 16 | 236 | 9811 | 0.0 | 37.8 | DEGRADED |
| rep-warm-n1-m200-r1 | 1 | 200 | 16/200 | 196 | 278 | 278 | 278 | 12.9 | 120.9 | 16 | 16 | 248 | 9779 | 0.0 | n/a | DEGRADED |
| rep-warm-n1-m200-r2 | 1 | 200 | 16/200 | 234 | 298 | 298 | 298 | 12.9 | 46.4 | 16 | 16 | 247 | 9798 | 0.0 | n/a | DEGRADED |
| rep-warm-n1-m200-r3 | 1 | 200 | 16/200 | 235 | 319 | 319 | 319 | 16.9 | 63.9 | 16 | 16 | 252 | 9794 | 0.0 | n/a | DEGRADED |
| rep-frozen-n10-m1-r1 | 10 | 1 | 1/1 | 568 | 568 | 568 | 568 | 2.0 | 2.2 | 1 | 1 | 14 | 9909 | 0.0 | 14.2 | OK |
| rep-frozen-n10-m1-r2 | 10 | 1 | 1/1 | 653 | 653 | 653 | 653 | 1.5 | 1.7 | 1 | 1 | 14 | 9918 | 0.0 | 13.6 | OK |
| rep-frozen-n10-m1-r3 | 10 | 1 | 1/1 | 537 | 537 | 537 | 537 | 1.5 | 1.8 | 1 | 1 | 14 | 9917 | 0.0 | 13.7 | OK |
| rep-warm-n10-m1-r1 | 10 | 1 | 1/1 | 14 | 14 | 14 | 14 | 1.5 | 1.6 | 1 | 1 | 15 | 9922 | 0.0 | n/a | OK |
| rep-warm-n10-m1-r2 | 10 | 1 | 1/1 | 12 | 12 | 12 | 12 | 1.4 | 1.7 | 1 | 1 | 15 | 9922 | 0.0 | n/a | OK |
| rep-warm-n10-m1-r3 | 10 | 1 | 1/1 | 13 | 13 | 13 | 13 | 1.5 | 2.0 | 1 | 1 | 15 | 9922 | 0.0 | n/a | OK |
| rep-frozen-n10-m10-r1 | 10 | 10 | 10/10 | 760 | 1104 | 1104 | 1104 | 1.9 | 34.0 | 10 | 10 | 133 | 9868 | 0.0 | 25.9 | OK |
| rep-frozen-n10-m10-r2 | 10 | 10 | 10/10 | 779 | 1124 | 1124 | 1124 | 2.1 | 44.2 | 10 | 10 | 144 | 9855 | 0.0 | 30.3 | OK |
| rep-frozen-n10-m10-r3 | 10 | 10 | 10/10 | 990 | 1330 | 1330 | 1330 | 2.0 | 53.0 | 10 | 10 | 142 | 9854 | 0.0 | 25.9 | OK |
| rep-warm-n10-m10-r1 | 10 | 10 | 10/10 | 60 | 71 | 71 | 71 | 9.7 | 23.0 | 10 | 10 | 148 | 9857 | 0.0 | n/a | OK |
| rep-warm-n10-m10-r2 | 10 | 10 | 10/10 | 55 | 81 | 81 | 81 | 8.6 | 37.8 | 10 | 10 | 150 | 9872 | 0.0 | n/a | OK |
| rep-warm-n10-m10-r3 | 10 | 10 | 10/10 | 62 | 75 | 75 | 75 | 10.7 | 26.8 | 10 | 10 | 150 | 9863 | 0.0 | n/a | OK |
| rep-frozen-n10-m50 | 10 | 50 | skipped | | | | | | | | 50 (est) | | 9874 | | | skipped: estimated 50 machine(s) × 256 MiB = 12800 MiB; available 9874 MiB (70% = 6911), disk free 596200 MiB |
| rep-warm-n10-m50 | 10 | 50 | skipped | | | | | | | | 50 (est) | | 9874 | | | skipped: estimated 50 machine(s) × 256 MiB = 12800 MiB; available 9874 MiB (70% = 6911), disk free 596200 MiB |
| rep-frozen-n10-m200 | 10 | 200 | skipped | | | | | | | | 160 (est) | | 9874 | | | skipped: estimated 160 machine(s) × 256 MiB = 40960 MiB; available 9874 MiB (70% = 6911), disk free 596200 MiB |
| rep-warm-n10-m200 | 10 | 200 | skipped | | | | | | | | 160 (est) | | 9874 | | | skipped: estimated 160 machine(s) × 256 MiB = 40960 MiB; available 9874 MiB (70% = 6911), disk free 596200 MiB |
| rep-frozen-n50-m1-r1 | 50 | 1 | 1/1 | 743 | 743 | 743 | 743 | 1.7 | 2.7 | 1 | 1 | 15 | 9922 | 0.0 | 14.5 | OK |
| rep-frozen-n50-m1-r2 | 50 | 1 | 1/1 | 701 | 701 | 701 | 701 | 1.5 | 1.7 | 1 | 1 | 14 | 9922 | 0.0 | 13.5 | OK |
| rep-frozen-n50-m1-r3 | 50 | 1 | 1/1 | 808 | 808 | 808 | 808 | 1.5 | 1.8 | 1 | 1 | 14 | 9923 | 0.0 | 12.2 | OK |
| rep-warm-n50-m1-r1 | 50 | 1 | 1/1 | 12 | 12 | 12 | 12 | 1.4 | 1.6 | 1 | 1 | 15 | 9928 | 0.0 | n/a | OK |
| rep-warm-n50-m1-r2 | 50 | 1 | 1/1 | 13 | 13 | 13 | 13 | 1.6 | 1.8 | 1 | 1 | 15 | 9927 | 0.0 | n/a | OK |
| rep-warm-n50-m1-r3 | 50 | 1 | 1/1 | 12 | 12 | 12 | 12 | 1.6 | 1.7 | 1 | 1 | 15 | 9928 | 0.0 | n/a | OK |
| rep-frozen-n50-m10-r1 | 50 | 10 | 10/10 | 1020 | 1461 | 1461 | 1461 | 2.3 | 6.3 | 10 | 10 | 143 | 9867 | 0.0 | 30.5 | OK |
| rep-frozen-n50-m10-r2 | 50 | 10 | 10/10 | 1329 | 2478 | 2478 | 2478 | 1.7 | 2.2 | 10 | 10 | 147 | 9867 | 0.0 | 29.5 | OK |
| rep-frozen-n50-m10-r3 | 50 | 10 | 10/10 | 1230 | 2315 | 2315 | 2315 | 1.7 | 4.6 | 10 | 10 | 144 | 9866 | 0.0 | 28.7 | OK |
| rep-warm-n50-m10-r1 | 50 | 10 | 10/10 | 55 | 96 | 96 | 96 | 8.4 | 28.4 | 10 | 10 | 150 | 9862 | 0.0 | n/a | OK |
| rep-warm-n50-m10-r2 | 50 | 10 | 10/10 | 74 | 98 | 98 | 98 | 10.2 | 25.8 | 10 | 10 | 150 | 9862 | 0.0 | n/a | OK |
| rep-warm-n50-m10-r3 | 50 | 10 | 10/10 | 58 | 101 | 101 | 101 | 7.5 | 44.2 | 10 | 10 | 151 | 9855 | 0.0 | n/a | OK |
| rep-frozen-n50-m50 | 50 | 50 | skipped | | | | | | | | 50 (est) | | 9858 | | | skipped: estimated 50 machine(s) × 256 MiB = 12800 MiB; available 9858 MiB (70% = 6900), disk free 596187 MiB |
| rep-warm-n50-m50 | 50 | 50 | skipped | | | | | | | | 50 (est) | | 9858 | | | skipped: estimated 50 machine(s) × 256 MiB = 12800 MiB; available 9858 MiB (70% = 6900), disk free 596187 MiB |
| rep-frozen-n50-m200 | 50 | 200 | skipped | | | | | | | | 200 (est) | | 9858 | | | skipped: estimated 200 machine(s) × 256 MiB = 51200 MiB; available 9858 MiB (70% = 6900), disk free 596187 MiB |
| rep-warm-n50-m200 | 50 | 200 | skipped | | | | | | | | 200 (est) | | 9858 | | | skipped: estimated 200 machine(s) × 256 MiB = 51200 MiB; available 9858 MiB (70% = 6900), disk free 596187 MiB |
