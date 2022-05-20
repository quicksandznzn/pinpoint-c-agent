#!/usr/bin/env python
# -*- coding: UTF-8 -*-

# ------------------------------------------------------------------------------
#  Copyright  2020. NAVER Corp.                                                -
#                                                                              -
#  Licensed under the Apache License, Version 2.0 (the "License");             -
#  you may not use this file except in compliance with the License.            -
#  You may obtain a copy of the License at                                     -
#                                                                              -
#   http://www.apache.org/licenses/LICENSE-2.0                                 -
#                                                                              -
#  Unless required by applicable law or agreed to in writing, software         -
#  distributed under the License is distributed on an "AS IS" BASIS,           -
#  WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.    -
#  See the License for the specific language governing permissions and         -
#  limitations under the License.                                              -
# ------------------------------------------------------------------------------

import importlib
def __monkey_patch(*args, **kwargs):
    for key in kwargs:
        if kwargs[key]:
            module = importlib.import_module('pinpointPy.Fastapi.' + key)
            monkey_patch = getattr(module, 'monkey_patch')
            if callable(monkey_patch):
                monkey_patch()
                print("try to install pinpointPy.asynlibs.%s module" % (key))

def asyn_monkey_patch_for_pinpoint(AioRedis=True):
    __monkey_patch(aioredis=AioRedis)


from .middleware import PinPointMiddleWare
from .AsyCommonPlugin import CommonPlugin
__all__ = ['asyn_monkey_patch_for_pinpoint', 'PinPointMiddleWare', 'CommonPlugin']