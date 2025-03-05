[![Gitbook Documentation](https://img.shields.io/badge/GitBook-Documentation-f37f40?style=plastic&logo=gitbook&logoColor=white&style=flat)](https://docs.ergo.services/extra-library/loggers)
[![MIT license](https://img.shields.io/badge/license-MIT-brightgreen.svg)](https://opensource.org/licenses/MIT)
[![Telegram Community](https://img.shields.io/badge/Telegram-ergo__services-229ed9?style=flat&logo=telegram&logoColor=white)](https://t.me/ergo_services)
[![Twitter](https://img.shields.io/badge/Twitter-ergo__services-00acee?style=flat&logo=twitter&logoColor=white)](https://twitter.com/ergo_services)
[![Reddit](https://img.shields.io/badge/Reddit-r/ergo__services-ff4500?style=plastic&logo=reddit&logoColor=white&style=flat)](https://reddit.com/r/ergo_services)

Extra library of loggers for the Ergo Framework 3.0 (and above)

## colored
Enables colorized output of log messages. Highlights log levels, values of `gen.Atom`, `gen.PID`, `gen.ProcessID`, `gen.Alias`, `gen.Ref`, and `gen.Event`.
Don't forget to disable the default logger `gen.NodeOptions.Log.DefaultLogger.Disable' in order to get rid of duplicate log messages on stdout.

![image](https://github.com/ergo-services/logger/assets/118860/bbe38476-a507-45d4-b430-e98eb41a188a)

**Warning**: not intended to be used for intensive logging.

Doc: https://docs.ergo.services/extra-library/loggers/colored

## rotate
This logger writes log messages to the file and makes a rotation of them.

Doc: https://docs.ergo.services/extra-library/loggers/rotate

