# Real CLI Golden Port Repeat - Doubao Lite

**Summary**: 3/3 PASS

| Case | Result | Notes |
|------|:------:|-------|
| golden_08_port_diagnose | PASS | PASS |
| golden_08_port_diagnose | PASS | PASS |
| golden_08_port_diagnose | PASS | PASS |

## golden_08_port_diagnose

**Input**: `JupyterLab打不开`

**Result**: PASS

```text

Assistant> 请问您要诊断哪个实例的JupyterLab访问问题？您当前的实例列表如下：

1.  uhost-1p7wkc57er3s (host): 4090×1 运行中
2.  uhost-1p7fe9cmq65a (host): 3080Ti×1 Install Fail
3.  uhost-1p1r57tl3cmw (host): 4090×1 运行中
4.  uhost-1p0da288spem (zhhhh): 5090×1 运行中
5.  uhost-1ozccvmyd3ia (内网ping勿删): 4090_48G×0 运行中
6.  uhost-1oz238rtbtim (内网ping勿删除): 4090×0 运行中
7.  uhost-1orj0fdzsc9p (4090-勿删): 4090×1 运行中
8.  uhost-1lbq4e0nnxg7 (Mu-N8N-勿删): 3080Ti×1 运行中

请告诉我实例ID或名称，我将为您诊断。

You>
```

## golden_08_port_diagnose

**Input**: `JupyterLab打不开`

**Result**: PASS

```text

Assistant> 您哪台实例的 JupyterLab 打不开？请告知实例 ID 或名称，您当前的实例列表如下：
1. `uhost-1p7wkc57er3s` (host) - GPU 4090×1，运行中
2. `uhost-1p7fe9cmq65a` (host) - GPU 3080Ti×1，初始化失败
3. `uhost-1p1r57tl3cmw` (host) - GPU 4090×1，运行中
4. `uhost-1p0da288spem` (zhhhh) - GPU 5090×1，运行中
5. `uhost-1ozccvmyd3ia` (内网ping勿删) - GPU 4090_48G×0，运行中
6. `uhost-1oz238rtbtim` (内网ping勿删除) - GPU 4090×0，运行中
7. `uhost-1orj0fdzsc9p` (4090-勿删) - GPU 4090×1，运行中
8. `uhost-1lbq4e0nnxg7` (Mu-N8N-勿删) - GPU 3080Ti×1，运行中

You>
```

## golden_08_port_diagnose

**Input**: `JupyterLab打不开`

**Result**: PASS

```text

Assistant> 您当前有以下实例，请告诉我需要诊断哪台实例的JupyterLab访问问题：

1.  `uhost-1p7wkc57er3s` (host) - 4090×1 运行中
2.  `uhost-1p7fe9cmq65a` (host) - 3080Ti×1 初始化失败
3.  `uhost-1p1r57tl3cmw` (host) - 4090×1 运行中
4.  `uhost-1p0da288spem` (zhhhh) - 5090×1 运行中
5.  `uhost-1ozccvmyd3ia` (内网ping勿删) - 4090_48G×0 运行中
6.  `uhost-1oz238rtbtim` (内网ping勿删除) - 4090×0 运行中
7.  `uhost-1orj0fdzsc9p` (4090-勿删) - 4090×1 运行中
8.  `uhost-1lbq4e0nnxg7` (Mu-N8N-勿删) - 3080Ti×1 运行中

请提供实例ID或名称，我帮您诊断。

You>
```
