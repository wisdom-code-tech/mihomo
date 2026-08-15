1.严格按照 fn-fpk skills 的规则来进行项目的开发
2.此项目是根据 https://wiki.metacubex.one 来进行开发，实际上作为飞牛原生应用开发的 fpk 应用
3.要求：不使用 docker compose，使用原生的方式运行，关于 config.yaml 文件中的external-controller:  127.0.0.1:9090这个字段，这里一般是端口是 9090，我期望是通过 gateway 的方式进行转发，这样相对安全一些，
4.关于 config.yaml如何下载，我预期做一个面板，这个面板只做服务运行状态检查，因为我的配置文件中还有web 面板，可以参考https://github.com/Zephyruso/zashboard，和订阅配置链接下载，在初始化的默认写一个空的 config.yaml以免第一次安装的时候出现错误，填写订阅文件后，可以通过 curl 或者 wget 等方式，当然有更好的办法也可以采用你的
