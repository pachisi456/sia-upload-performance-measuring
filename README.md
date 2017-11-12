# Sia Upload Performance Measuring

This repository is a clone of the [original Sia repository](https://github.com/NebulousLabs/Sia)
as of November 2017. It is modified such, that it measures and prints out times consumed by certain computational
steps when uploading. Moreover it prints the according thread-ID's, so that it can be observed how many threads are
started and thus how well the software parallelizes when running on multiple processors / cores. Following
computational steps are monitored:
* splitting of files into 40 MB chunks
* Reed-Solomon erasure coding
* Twofish encryption 


Therefore
[modules/renter/repairscanner.go](https://github.com/pachisi456/Sia/blob/master/modules/renter/repairscanner.go)
and [modules/renter/repairchunk.go](https://github.com/pachisi456/Sia/blob/master/modules/renter/repairchunk.go) have
been edited accordingly.
 
