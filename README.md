# FreeCache2

感谢 freecache 原作者对代码的无私贡献和其他开发者对代码的改善付出的卓越的努力。 

# 设计初心： 
freecache 因为其高效的性能获得很多人的喜爱，我也不例外。却因为里面使用了go Mutex导致了在非常高的并发情况下性能无法进一步提高，主要原因就是使用 get 方法中的Mutex.Lock()。 通过跟原作者的沟通后，发现因为 freecache get方法并不是只读方法，所以才动了念头修改一版能够满足自己高并发http平台的cache。

# 特性 （相比原 freecache）：
1. 将Mutex 修改成 RWMutex  --> 并发性能极大的提高
2. 去掉了访问时间计数器

# 使用例子： （和原 freecache 没有区别）

cacheSize := 100 * 1024 * 1024
cache := freecache.NewCache(cacheSize)
debug.SetGCPercent(20)
key := []byte("abc")
val := []byte("def")
expire := 60 // expire in 60 seconds
cache.Set(key, val, expire)
got, err := cache.Get(key)
if err != nil {
    fmt.Println(err)
} else {
    fmt.Println(string(got))
}
affected := cache.Del(key)
fmt.Println("deleted key ", affected)
fmt.Println("entry count ", cache.EntryCount())
