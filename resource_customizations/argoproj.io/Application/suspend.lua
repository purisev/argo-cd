if obj.metadata.annotations == nil then
  obj.metadata.annotations = {}
end
obj.metadata.annotations["argocd.argoproj.io/suspend"] = "true"
return obj
