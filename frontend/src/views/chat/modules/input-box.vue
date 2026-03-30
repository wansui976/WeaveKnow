<script setup lang="ts">
const chatStore = useChatStore();
const { connectionStatus, input, isRateLimited, list, rateLimitRemainingSeconds, wsData } = storeToRefs(chatStore);

function appendLiveEvent(assistant: Api.Chat.Message, event: Api.Chat.LiveEvent) {
  const nextEvents = [...(assistant.events || [])];
  const lastEvent = nextEvents[nextEvents.length - 1];

  if (
    lastEvent
    && lastEvent.type === event.type
    && lastEvent.message === event.message
    && lastEvent.stage === event.stage
    && lastEvent.tool === event.tool
  ) {
    return;
  }

  nextEvents.push(event);
  assistant.events = nextEvents.slice(-8);
}

function resolveProgressMessage(stage?: string) {
  if (stage === 'retrieving') {
    return '正在检索知识库...';
  }

  if (stage === 'answering') {
    return '正在生成答案...';
  }

  return '正在分析问题...';
}

function buildWsErrorMessage(data: Record<string, any>) {
  if (data.code === 429) {
    const retryAfterSeconds = Number(data.retryAfterSeconds || 0);
    const baseMessage = data.message || '聊天请求过于频繁';

    if (retryAfterSeconds > 0) {
      return `${baseMessage}，请在 ${retryAfterSeconds} 秒后重试`;
    }

    return `${baseMessage}，请稍后再试`;
  }

  if (typeof data.error === 'string' && data.error.trim()) {
    return data.error.trim();
  }

  if (typeof data.message === 'string' && data.message.trim()) {
    return data.message.trim();
  }

  return '服务器繁忙，请稍后再试';
}

const latestMessage = computed(() => {
  return list.value[list.value.length - 1] ?? {};
});

const isSending = computed(() => {
  return (
    latestMessage.value?.role === 'assistant' && ['loading', 'pending'].includes(latestMessage.value?.status || '')
  );
});

const sendDisabled = computed(() => {
  if (isSending.value) {
    return false;
  }

  if (isRateLimited.value) {
    return true;
  }

  return !input.value.message || ['CLOSED', 'CONNECTING'].includes(connectionStatus.value);
});

const connectionText = computed(() => {
  if (connectionStatus.value === 'OPEN') {
    return '已连接';
  }

  if (connectionStatus.value === 'RECONNECTING') {
    return '重连中';
  }

  if (connectionStatus.value === 'CONNECTING') {
    return '连接中';
  }

  return '未连接';
});

const cooldownText = computed(() => {
  if (!isRateLimited.value) {
    return '';
  }

  return `${rateLimitRemainingSeconds.value} 秒后可重新发送`;
});

watch(wsData, val => {
  if (!val) return;

  let payload: Record<string, any>;

  try {
    payload = JSON.parse(val);
  } catch {
    return;
  }

  const assistant = list.value[list.value.length - 1];

  if (!assistant) return;

  if (payload.type === 'completion' && payload.status === 'finished' && assistant.status !== 'error') {
    assistant.status = 'finished';
    assistant.progressText = '';
    assistant.progressStage = undefined;
  }

  if (payload.type === 'progress') {
    assistant.status = 'loading';
    assistant.progressStage = payload.stage;
    assistant.progressText = payload.message || resolveProgressMessage(payload.stage);
    appendLiveEvent(assistant, {
      type: 'progress',
      stage: payload.stage,
      message: assistant.progressText,
      timestamp: Number(payload.timestamp || Date.now())
    });
    return;
  }

  if (payload.type === 'tool_call') {
    assistant.status = 'loading';
    assistant.progressStage = 'retrieving';
    assistant.progressText = payload.message || '正在调用工具...';
    appendLiveEvent(assistant, {
      type: 'tool_call',
      tool: payload.tool,
      displayName: payload.displayName,
      query: payload.query,
      topK: Number(payload.topK || 0) || undefined,
      message: assistant.progressText,
      timestamp: Number(payload.timestamp || Date.now())
    });
    return;
  }

  if (payload.type === 'tool_result') {
    assistant.status = 'loading';
    assistant.progressText = payload.message || '工具执行完成';
    appendLiveEvent(assistant, {
      type: 'tool_result',
      tool: payload.tool,
      displayName: payload.displayName,
      message: assistant.progressText,
      success: Boolean(payload.success),
      resultCount: Number(payload.resultCount || 0),
      totalCount: Number(payload.totalCount || 0),
      timestamp: Number(payload.timestamp || Date.now())
    });
    return;
  }

  if (payload.type === 'sources' && Array.isArray(payload.sources)) {
    assistant.referenceMappings = payload.sources.reduce((acc: Record<string, Api.Chat.ReferenceEvidence>, item: Record<string, any>) => {
      const index = String(item.index || '');
      if (!index) return acc;

      acc[index] = {
        fileMd5: String(item.fileMd5 || ''),
        fileName: String(item.fileName || ''),
        anchorText: String(item.snippet || ''),
        evidenceSnippet: String(item.snippet || ''),
        matchedChunkText: String(item.snippet || ''),
        score: typeof item.score === 'number' ? item.score : Number(item.score || 0),
        chunkId: typeof item.chunkId === 'number' ? item.chunkId : Number(item.chunkId || 0)
      };
      return acc;
    }, {});
    return;
  }

  if (payload.error || Number(payload.code) >= 400) {
    if (Number(payload.code) === 429) {
      chatStore.startRateLimitCountdown(Number(payload.retryAfterSeconds || 0));
    }

    const message = buildWsErrorMessage(payload);

    assistant.status = 'error';
    assistant.content = message;
    assistant.progressText = '';
    assistant.progressStage = undefined;

    if (Number(payload.code) === 429) {
      window.$message?.warning(message);
    } else {
      window.$message?.error(message);
    }
  } else if (payload.chunk) {
    assistant.status = 'loading';
    assistant.progressStage = 'answering';
    assistant.progressText = '正在生成答案...';
    assistant.content += payload.chunk;
  }
});

const handleSend = async () => {
  if (isRateLimited.value) {
    window.$message?.warning(`当前发送受限，${cooldownText.value}`);
    return;
  }

  //  判断是否正在发送, 如果发送中，则停止ai继续响应
  if (isSending.value) {
    const { error, data: tokenData } = await request<Api.Chat.Token>({ url: 'chat/websocket-token', baseURL: 'proxy-api' });
    if (error) return;

    chatStore.wsSend(JSON.stringify({ type: 'stop', _internal_cmd_token: tokenData.cmdToken }));

    list.value[list.value.length - 1].status = 'finished';
    latestMessage.value.progressText = '';
    latestMessage.value.progressStage = undefined;
    if (!latestMessage.value.content && !latestMessage.value.events?.length) list.value.pop();
    return;
  }

  list.value.push({
    content: input.value.message,
    role: 'user'
  });
  chatStore.wsSend(input.value.message);
  list.value.push({
    content: '',
    role: 'assistant',
    status: 'pending',
    progressStage: 'planning',
    progressText: '正在分析问题...',
    events: [
      {
        type: 'progress',
        stage: 'planning',
        message: '正在分析问题...',
        timestamp: Date.now()
      }
    ]
  });
  input.value.message = '';
};

const inputRef = ref();
// 手动插入换行符（确保所有浏览器兼容）
const insertNewline = () => {
  const textarea = inputRef.value;
  const start = textarea.selectionStart;
  const end = textarea.selectionEnd;

  // 在光标位置插入换行符
  input.value.message = `${input.value.message.substring(0, start)}\n${input.value.message.substring(end)}`;

  // 更新光标位置（在插入的换行符之后）
  nextTick(() => {
    textarea.selectionStart = start + 1;
    textarea.selectionEnd = start + 1;
    textarea.focus(); // 确保保持焦点
  });
};

// ctrl + enter 换行
// enter 发送
const handShortcut = (e: KeyboardEvent) => {
  if (e.key === 'Enter') {
    e.preventDefault();

    if (!e.shiftKey && !e.ctrlKey) {
      handleSend();
    } else insertNewline();
  }
};
</script>

<template>
  <div class="relative w-full b-1 b-#1c1c1c20 bg-#fff p-4 card-wrapper dark:bg-#1c1c1c">
    <textarea
      ref="inputRef"
      v-model.trim="input.message"
      placeholder="给 派聪明 发送消息"
      class="min-h-10 w-full cursor-text resize-none b-none bg-transparent color-#333 caret-[rgb(var(--primary-color))] outline-none dark:color-#f1f1f1"
      @keydown="handShortcut"
    />
    <div class="flex items-center justify-between pt-2">
      <div class="flex items-center gap-3 text-18px color-gray-500">
        <NText class="text-14px">连接状态：</NText>
        <icon-eos-icons:loading v-if="connectionStatus === 'CONNECTING' || connectionStatus === 'RECONNECTING'" class="color-yellow" />
        <icon-fluent:plug-connected-checkmark-20-filled v-else-if="connectionStatus === 'OPEN'" class="color-green" />
        <icon-tabler:plug-connected-x v-else class="color-red" />
        <NText class="text-14px">{{ connectionText }}</NText>
        <NText v-if="isRateLimited" type="warning" class="text-13px">{{ cooldownText }}</NText>
      </div>
      <NButton :disabled="sendDisabled" strong circle type="primary" @click="handleSend">
        <template #icon>
          <icon-material-symbols:stop-rounded v-if="isSending" />
          <icon-guidance:send v-else />
        </template>
      </NButton>
    </div>
  </div>
</template>

<style scoped></style>
