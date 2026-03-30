<script setup lang="ts">
// eslint-disable-next-line @typescript-eslint/no-unused-vars
import { nextTick } from 'vue';
import { formatDate } from '@/utils/common';
import { router } from '@/router';
import { request } from '@/service/request';
import { VueMarkdownIt } from '@/vendor/vue-markdown-shiki';
defineOptions({ name: 'ChatMessage' });

const props = defineProps<{
  msg: Api.Chat.Message,
  sessionId?: string,
  retrievalQueryFallback?: string
}>();

const authStore = useAuthStore();

function handleCopy(content: string) {
  navigator.clipboard.writeText(content);
  window.$message?.success('已复制');
}

const chatStore = useChatStore();

interface ParsedReferenceEntry {
  fileName: string;
  label: string;
  referenceNumber: number;
  fileMd5?: string;
  pageNumber?: number;
}

const bareUrlPattern = /https?:\/\/[A-Za-z0-9\-._~:/?#\[\]@!$&'()*+,;=%]+/g;

function getReferenceDetail(referenceNumber: string | number) {
  return props.msg.referenceMappings?.[String(referenceNumber)] || props.msg.referenceMappings?.[referenceNumber];
}

function splitTrailingUrlPunctuation(rawUrl: string) {
  let url = rawUrl;
  let trailing = '';

  while (url) {
    const lastChar = url.at(-1);
    if (!lastChar) break;

    if (/[，。！？；：、,.!?;:]/.test(lastChar)) {
      trailing = `${lastChar}${trailing}`;
      url = url.slice(0, -1);
      continue;
    }

    if (lastChar === ')' || lastChar === '）') {
      const openingChar = lastChar === ')' ? '(' : '（';
      const closingChar = lastChar;
      const openingCount = (url.match(new RegExp(`\\${openingChar}`, 'g')) || []).length;
      const closingCount = (url.match(new RegExp(`\\${closingChar}`, 'g')) || []).length;

      if (closingCount > openingCount) {
        trailing = `${lastChar}${trailing}`;
        url = url.slice(0, -1);
        continue;
      }
    }

    break;
  }

  return { url, trailing };
}

function normalizeBareUrls(text: string) {
  return text.replace(bareUrlPattern, (match, offset: number, source: string) => {
    const previousChar = source[offset - 1] || '';
    const previousTwoChars = source.slice(Math.max(0, offset - 2), offset);
    const previousTenChars = source.slice(Math.max(0, offset - 10), offset).toLowerCase();

    if (previousChar === '<' || previousTwoChars === '](' || /(?:href|src)=["']?$/.test(previousTenChars)) {
      return match;
    }

    const { url, trailing } = splitTrailingUrlPunctuation(match);
    return url ? `<${url}>${trailing}` : match;
  });
}

function createReferenceAnchor(referenceNumber: string | number, text: string, extraClass = ''): string {
  const className = ['source-file-link', extraClass].filter(Boolean).join(' ');
  return `<a href="#" class="${className}" data-ref="${referenceNumber}">${text}</a>`;
}

function createSourceLink(
  sourceNum: string,
  fileName: string,
  extras?: { displayName?: string }
): string {
  const trimmedFileName = fileName.trim();
  return `来源#${sourceNum}: ${createReferenceAnchor(sourceNum, extras?.displayName || trimmedFileName)}`;
}

// 处理来源文件链接的函数
function processSourceLinks(text: string): string {
  // 支持单个来源，也支持一个括号里包含多个来源：
  // (来源#1: test.pdf | 第5页; 来源#2: other.pdf | 第8页)
  const entryBoundary = '(?=\\s*(?:[;；,，、。！？!?\\)）]|$))';
  const pagePattern = new RegExp(
    `来源#(\\d+):\\s*([^|;；,，、。！？!?\\n\\r]+?)\\s*\\|\\s*第(\\d+)页${entryBoundary}`,
    'g'
  );
  const md5Pattern = new RegExp(
    `来源#(\\d+):\\s*([^|;；,，、。！？!?\\n\\r]+?)\\s*\\|\\s*MD5:\\s*([a-fA-F0-9]+)${entryBoundary}`,
    'g'
  );
  const simplePattern = new RegExp(
    `来源#(\\d+):\\s*([^<>\\n\\r|;；,，、。！？!?]+?)${entryBoundary}`,
    'g'
  );

  let processedText = text.replace(pagePattern, (_match, sourceNum, fileName, pageNum) => {
    return createSourceLink(sourceNum, fileName, {
      displayName: `${fileName.trim()} (第${pageNum}页)`
    });
  });

  processedText = processedText.replace(md5Pattern, (_match, sourceNum, fileName, _fileMd5) => {
    return createSourceLink(sourceNum, fileName, {
      displayName: fileName.trim()
    });
  });

  processedText = processedText.replace(simplePattern, (_match, sourceNum, fileName) => {
    return createSourceLink(sourceNum, fileName);
  });

  return processedText;
}

function processInlineReferenceLinks(text: string) {
  return text.replace(/\[(\d+)\](?!\()/g, (match, sourceNum) => {
    if (!getReferenceDetail(sourceNum) && !referenceEntries.value.some(entry => entry.referenceNumber === Number(sourceNum))) {
      return match;
    }
    return createReferenceAnchor(sourceNum, `[${sourceNum}]`, 'source-inline-link');
  });
}

function parseReferenceSection(rawContent: string): { mainContent: string; entries: ParsedReferenceEntry[] } {
  const normalized = rawContent.replace(/\r\n/g, '\n');
  const sectionPattern = /(?:^|\n)(?:#{1,6}\s*)?(引用来源|参考来源|参考文献|来源)[:：]\s*\n((?:\s*\[\d+\][^\n]*(?:\n|$))+)\s*$/;
  const match = normalized.match(sectionPattern);

  const parseLines = (block: string) => {
    return block
      .split('\n')
      .map(line => line.trim())
      .filter(Boolean)
      .map(line => {
        const lineMatch = line.match(/^\[(\d+)\]\s+(.+)$/);
        if (!lineMatch) return null;

        const referenceNumber = Number(lineMatch[1]);
        const label = lineMatch[2].trim();
        const persisted = getReferenceDetail(referenceNumber);
        return {
          referenceNumber,
          label,
          fileName: (persisted?.fileName || label).trim(),
          fileMd5: persisted?.fileMd5 || undefined,
          pageNumber: persisted?.pageNumber || undefined
        } satisfies ParsedReferenceEntry;
      })
      .filter((item): item is ParsedReferenceEntry => Boolean(item));
  };

  if (!match) {
    return {
      mainContent: normalized,
      entries: []
    };
  }

  const entries = parseLines(match[2] || '');
  const mainContent = normalized.slice(0, match.index).trimEnd();
  return { mainContent, entries };
}

const referenceSection = computed(() => parseReferenceSection(props.msg.content ?? ''));

const referenceEntries = computed(() => {
  if (referenceSection.value.entries.length) {
    return referenceSection.value.entries;
  }

  const mappings = props.msg.referenceMappings || {};
  return Object.entries(mappings)
    .map(([key, value]) => ({
      referenceNumber: Number(key),
      label: value.fileName,
      fileName: value.fileName,
      fileMd5: value.fileMd5 || undefined,
      pageNumber: value.pageNumber || undefined
    }))
    .filter(item => Number.isFinite(item.referenceNumber))
    .sort((a, b) => a.referenceNumber - b.referenceNumber);
});

function resolveReferenceEntry(referenceNumber: number) {
  return referenceEntries.value.find(entry => entry.referenceNumber === referenceNumber);
}

const content = computed(() => {
  chatStore.scrollToBottom?.();
  const rawContent = referenceSection.value.mainContent;

  if (props.msg.role === 'assistant') {
    return normalizeBareUrls(processInlineReferenceLinks(processSourceLinks(rawContent)));
  }

  return rawContent;
});

const assistantEvents = computed(() => props.msg.events || []);

const assistantProgressText = computed(() => {
  if (props.msg.progressText) {
    return props.msg.progressText;
  }

  if (props.msg.status === 'loading') {
    return '正在生成答案...';
  }

  if (props.msg.status === 'pending') {
    return '正在分析问题...';
  }

  if (assistantEvents.value.length) {
    return '处理完成';
  }

  return '';
});

const showAssistantProgress = computed(() => {
  if (props.msg.role !== 'assistant' || props.msg.status === 'error') {
    return false;
  }

  return Boolean(assistantProgressText.value || assistantEvents.value.length);
});

function progressTagType(event: Api.Chat.LiveEvent): 'default' | 'info' | 'success' | 'warning' | 'error' {
  if (event.type === 'tool_result') {
    return event.success === false ? 'error' : 'success';
  }

  if (event.stage === 'retrieving' || event.type === 'tool_call') {
    return 'warning';
  }

  if (event.stage === 'answering') {
    return 'success';
  }

  return 'info';
}

function progressTagText(event: Api.Chat.LiveEvent) {
  if (event.type === 'tool_call') {
    return event.displayName || '工具调用';
  }

  if (event.type === 'tool_result') {
    return event.success === false ? '工具失败' : '工具结果';
  }

  if (event.stage === 'retrieving') {
    return '正在检索';
  }

  if (event.stage === 'answering') {
    return '正在生成';
  }

  return '正在分析';
}

function extractContextAnchorText(target: HTMLElement) {
  const scope = target.closest('li, p, blockquote, td, th');
  const rawText = scope?.textContent?.replace(/\s+/g, ' ').trim() || '';
  if (!rawText) return '';

  const beforeCitation = rawText.split(/(?:\(|（)?来源#\d+:/)[0] || rawText;
  return beforeCitation
    .replace(/^\s*\d+\.\s*/, '')
    .replace(/[（(]\s*$/, '')
    .replace(/\s+/g, ' ')
    .trim();
}

function openReferencePreviewPage(payload: {
  retrievalMode?: Api.Chat.ReferenceEvidence['retrievalMode'];
  retrievalLabel?: string | null;
  retrievalQuery?: string | null;
  evidenceSnippet?: string | null;
  matchedChunkText?: string | null;
  score?: number | null;
  chunkId?: number | null;
  fileName: string;
  fileMd5?: string | null;
  pageNumber?: number | null;
  anchorText?: string | null;
  sessionId?: string;
  referenceNumber: number;
}) {
  const previewKey = `reference-preview:${Date.now()}:${Math.random().toString(36).slice(2, 8)}`;
  localStorage.setItem(previewKey, JSON.stringify(payload));

  const routeLocation = router.resolve({
    path: '/chat',
    query: {
      preview: 'reference',
      previewKey
    }
  });

  window.open(routeLocation.href, '_blank', 'noopener,noreferrer');
}

// 处理内容点击事件（事件委托）
function handleContentClick(event: MouseEvent) {
  const target = event.target as HTMLElement | null;
  const linkTarget = target?.closest?.('.source-file-link') as HTMLElement | null;

  // 检查点击的是否是文件链接
  if (linkTarget) {
    event.preventDefault();
    const referenceNumber = Number(linkTarget.getAttribute('data-ref') || 0);
    const entry = resolveReferenceEntry(referenceNumber);

    if (referenceNumber > 0) {
      const contextAnchorText = extractContextAnchorText(linkTarget);
      handleSourceFileClick({
        fileName: entry?.fileName || linkTarget.textContent?.trim() || '',
        referenceNumber,
        fileMd5: entry?.fileMd5,
        anchorText: contextAnchorText
      });
    }
  }
}

// 处理来源文件点击事件
async function handleSourceFileClick(fileInfo: {
  fileName: string;
  referenceNumber: number;
  fileMd5?: string;
  anchorText?: string;
}) {
  const { fileName, referenceNumber, fileMd5: extractedMd5, anchorText: clickedAnchorText } = fileInfo;
  const persistedDetail = props.msg.referenceMappings?.[String(referenceNumber)] || props.msg.referenceMappings?.[referenceNumber];
  const referenceSessionId = props.msg.conversationId || props.sessionId;
  console.log('点击了来源文件:', fileName, '引用编号:', referenceNumber, '提取的MD5:', extractedMd5, '会话ID:', referenceSessionId);

  try {
    let detail: Api.Document.ReferenceDetailResponse | null = null;
    const fallbackRetrievalQuery = props.retrievalQueryFallback || '';

    if (referenceSessionId && (!persistedDetail?.retrievalQuery || !persistedDetail?.matchedChunkText || !persistedDetail?.evidenceSnippet)) {
      try {
        const { error: detailError, data: detailData } = await request<Api.Document.ReferenceDetailResponse>({
          url: 'documents/reference-detail',
          params: {
            sessionId: referenceSessionId,
            referenceNumber: referenceNumber.toString()
          },
          baseURL: '/proxy-api'
        });

        if (!detailError && detailData?.fileMd5) {
          detail = detailData;
        }
      } catch (detailErr) {
        console.warn('通过API查询引用详情失败:', detailErr);
      }
    }

    if (persistedDetail?.fileMd5 && !detail) {
      openReferencePreviewPage({
        fileName: persistedDetail.fileName || fileName,
        fileMd5: persistedDetail.fileMd5,
        pageNumber: persistedDetail.pageNumber,
        anchorText: persistedDetail.anchorText || clickedAnchorText || '',
        retrievalMode: persistedDetail.retrievalMode,
        retrievalLabel: persistedDetail.retrievalLabel,
        retrievalQuery: persistedDetail.retrievalQuery || fallbackRetrievalQuery,
        evidenceSnippet: persistedDetail.evidenceSnippet,
        matchedChunkText: persistedDetail.matchedChunkText,
        score: persistedDetail.score,
        chunkId: persistedDetail.chunkId,
        sessionId: referenceSessionId,
        referenceNumber
      });
      return;
    }

    const targetMd5 = detail?.fileMd5 || extractedMd5 || null;
    openReferencePreviewPage({
      fileName: detail?.fileName || fileName,
      fileMd5: targetMd5,
      pageNumber: detail?.pageNumber,
      anchorText: detail?.anchorText || clickedAnchorText || '',
      retrievalMode: detail?.retrievalMode,
      retrievalLabel: detail?.retrievalLabel,
      retrievalQuery: detail?.retrievalQuery || fallbackRetrievalQuery,
      evidenceSnippet: detail?.evidenceSnippet,
      matchedChunkText: detail?.matchedChunkText,
      score: detail?.score,
      chunkId: detail?.chunkId,
      sessionId: referenceSessionId,
      referenceNumber
    });
  } catch (err) {
    console.error('文件下载失败:', err);
    window.$message?.error(`文件下载失败: ${fileName}`);
  }
}
</script>

<template>
  <div class="mb-8 flex-col gap-2">
    <div v-if="msg.role === 'user'" class="flex items-center gap-4">
      <NAvatar class="bg-success">
        <SvgIcon icon="ph:user-circle" class="text-icon-large color-white" />
      </NAvatar>
      <div class="flex-col gap-1">
        <NText class="text-4 font-bold">{{ authStore.userInfo.username }}</NText>
        <NText class="text-3 color-gray-500">{{ formatDate(msg.timestamp) }}</NText>
      </div>
    </div>
    <div v-else class="flex items-center gap-4">
      <NAvatar class="bg-primary">
        <SystemLogo class="text-6 text-white" />
      </NAvatar>
      <div class="flex-col gap-1">
        <NText class="text-4 font-bold">派聪明</NText>
        <NText class="text-3 color-gray-500">{{ formatDate(msg.timestamp) }}</NText>
      </div>
    </div>
    <NText v-if="msg.status === 'error'" class="ml-12 mt-2 italic color-#d03050">
      {{ msg.content || '服务器繁忙，请稍后再试' }}
    </NText>
    <div v-else-if="msg.role === 'assistant'" class="mt-2 pl-12" @click="handleContentClick">
      <div v-if="showAssistantProgress" class="mb-3 rounded-3 bg-#f8fafc px-4 py-3 dark:bg-#202020">
        <div class="flex items-center gap-2 text-14px font-600">
          <icon-eos-icons:loading
            v-if="msg.status === 'pending' || msg.status === 'loading'"
            class="text-18px color-[rgb(var(--primary-color))]"
          />
          <icon-mingcute:check-fill v-else class="text-16px color-#18a058" />
          <span>{{ assistantProgressText }}</span>
        </div>
        <div v-if="assistantEvents.length" class="mt-3 flex flex-col gap-2">
          <div
            v-for="(event, index) in assistantEvents"
            :key="`${event.type}-${event.timestamp || index}`"
            class="flex items-start gap-2 rounded-2 bg-white px-3 py-2 text-13px dark:bg-#161616"
          >
            <NTag size="small" :type="progressTagType(event)">{{ progressTagText(event) }}</NTag>
            <span class="flex-1 break-all color-gray-600 dark:color-gray-300">{{ event.message }}</span>
          </div>
        </div>
      </div>
      <NText v-if="!content && (msg.status === 'pending' || msg.status === 'loading')" class="text-13px color-gray-500">
        <icon-eos-icons:three-dots-loading class="text-7" />
      </NText>
      <VueMarkdownIt v-if="content" :content="content" />
      <div v-if="referenceEntries.length" class="reference-panel">
        <div class="reference-panel__title">引用来源</div>
        <ul class="reference-panel__list">
          <li v-for="entry in referenceEntries" :key="entry.referenceNumber" class="reference-panel__item">
            <a
              href="#"
              class="source-file-link reference-panel__link"
              :data-ref="entry.referenceNumber"
            >
              <span class="source-ref-index">[{{ entry.referenceNumber }}]</span>
              <span>{{ entry.label }}</span>
            </a>
          </li>
        </ul>
      </div>
    </div>
    <NText v-else-if="msg.role === 'user'" class="ml-12 mt-2 text-4">{{ content }}</NText>
    <NDivider class="ml-12 w-[calc(100%-3rem)] mb-0! mt-2!" />
    <div class="ml-12 flex gap-4">
      <NButton quaternary @click="handleCopy(msg.content)">
        <template #icon>
          <icon-mynaui:copy />
        </template>
      </NButton>
    </div>
  </div>
</template>

<style scoped lang="scss">
:deep(.source-ref-index) {
  margin-right: 4px;
  color: #666;
  font-weight: 600;
}

:deep(.source-file-link) {
  color: #1890ff;
  cursor: pointer;
  text-decoration: underline;
  transition: color 0.2s;

  &:hover {
    color: #40a9ff;
    text-decoration: none;
  }

  &:active {
    color: #096dd9;
  }
}
</style>
