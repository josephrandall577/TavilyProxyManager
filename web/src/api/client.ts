import axios from 'axios'
import { ref } from 'vue'

const STORAGE_KEY = 'tavily_proxy_master_key'

const masterKeyRef = ref<string>(localStorage.getItem(STORAGE_KEY) ?? '')

export function getMasterKey(): string {
  return masterKeyRef.value
}

export function setMasterKey(value: string): void {
  localStorage.setItem(STORAGE_KEY, value)
  masterKeyRef.value = value
}

export function clearMasterKey(): void {
  localStorage.removeItem(STORAGE_KEY)
  masterKeyRef.value = ''
}

export const api = axios.create()

api.interceptors.request.use((config) => {
  const token = getMasterKey()
  if (token) {
    config.headers = config.headers ?? {}
    config.headers.Authorization = `Bearer ${token}`
  }
  return config
})

api.interceptors.response.use(
  (response) => response,
  (error) => {
    if (error?.response?.status === 401) {
      const sentAuthorization = error.config?.headers?.get?.('Authorization')
        ?? error.config?.headers?.Authorization
      if (sentAuthorization && sentAuthorization !== `Bearer ${getMasterKey()}`) {
        return Promise.reject(error)
      }
      clearMasterKey()
      window.dispatchEvent(new Event('auth-required'))
    }
    return Promise.reject(error)
  }
)
